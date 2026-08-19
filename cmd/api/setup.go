package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"erawan-cluster/internal/cluster/core"
	mysqlcluster "erawan-cluster/internal/cluster/mysql"
	mysqldbmanager "erawan-cluster/internal/cluster/mysql/dbmanager"
	pgsqlcluster "erawan-cluster/internal/cluster/pgsql"
	"erawan-cluster/internal/cluster/pgsql/dbmanager"
	"erawan-cluster/internal/haproxy"
	"erawan-cluster/internal/security"

	_ "github.com/lib/pq"
)

/**
 * buildApplication wires every subsystem from resolved configuration and returns
 * an application ready to serve HTTP. It fails fast: if any dependency cannot be
 * initialised it returns an error (wrapped with context) so main can abort
 * start-up cleanly. This is the single place where the engines are assembled —
 * adding a new engine means loading its clusterEngineConfig, adding a
 * buildXCluster helper, and attaching the service here.
 *
 * Params:
 *   ctx context.Context - the process base context; handed to each engine's
 *     background job runner so a shutdown cancels in-flight Ansible runs.
 *   cfg runtimeConfig - the fully-resolved process configuration.
 * Returns:
 *   *application - the assembled dependency container, on success.
 *   error - the first subsystem init/validation failure, wrapped with context.
 */
func buildApplication(ctx context.Context, cfg runtimeConfig) (*application, error) {
	if err := validateSecurityConfig(cfg); err != nil {
		return nil, err
	}

	jobDB, err := buildJobDB(ctx, cfg.dbConnection, cfg.dbPool)
	if err != nil {
		return nil, err
	}

	haproxySvc, err := buildHAProxy(ctx, cfg.haproxy, jobDB)
	if err != nil {
		if jobDB != nil {
			_ = jobDB.Close()
		}
		return nil, err
	}

	mysqlStore, mysqlSvc, err := buildMySQLCluster(ctx, cfg.mysql, cfg.ssh, cfg.server.proxyHost, cfg.maxConcurrentJobs, jobDB)
	if err != nil {
		if jobDB != nil {
			_ = jobDB.Close()
		}
		return nil, err
	}

	pgsqlStore, pgsqlSvc, err := buildPGSQLCluster(ctx, cfg.pgsql, cfg.ssh, cfg.controlPlane.ForEngine("pgsql"), cfg.server.proxyHost, cfg.maxConcurrentJobs, jobDB)
	if err != nil {
		if jobDB != nil {
			_ = jobDB.Close()
		}
		return nil, err
	}

	cipher, err := buildCipher(cfg.encryptionKey)
	if err != nil {
		return nil, err
	}

	return &application{
		config:               cfg.server,
		haproxy:              haproxySvc,
		mysqlCluster:         mysqlSvc,
		pgsqlCluster:         pgsqlSvc,
		pgsqlDB:              dbmanager.NewService(pgsqlStore, cfg.server.proxyHost),
		mysqlDB:              mysqldbmanager.NewService(mysqlStore, cfg.server.proxyHost),
		cipher:               cipher,
		enablePprof:          cfg.enablePprof,
		shutdownDrainSeconds: cfg.shutdownDrainSeconds,
		jobDB:                jobDB,
	}, nil
}

/**
 * validateSecurityConfig fails start-up closed when the API is unauthenticated
 * outside development (see security.ValidateAuthConfig).
 *
 * Params:
 *   cfg runtimeConfig - the resolved configuration; only env and apiKey are read.
 * Returns:
 *   error - non-nil when ENV != dev and no API key is set; nil otherwise.
 */
func validateSecurityConfig(cfg runtimeConfig) error {
	return security.ValidateAuthConfig(cfg.server.env, cfg.server.apiKey)
}

/**
 * buildHAProxy constructs the HAProxy service that renders per-tenant config
 * fragments and reloads the proxy. When a database handle is provided it wires
 * a DBConfigStore so configs survive a VIP failover: the new active node calls
 * Reconcile() to restore any configs that are in the DB but missing on disk.
 *
 * Params:
 *   ctx context.Context - used for Reconcile and schema setup.
 *   cfg haproxyConfig   - tenants dir, reload command/timeout and main configs.
 *   db  *sql.DB         - optional; when non-nil, configs are persisted to DB.
 * Returns:
 *   *haproxy.Service - the configured service, on success.
 *   error - if the tenants directory or DB schema cannot be initialised.
 */
func buildHAProxy(ctx context.Context, cfg haproxyConfig, db *sql.DB) (*haproxy.Service, error) {
	svc, err := haproxy.NewService(cfg.tenantsDir, cfg.reloadCmd, cfg.reloadTimeout)
	if err != nil {
		return nil, fmt.Errorf("init haproxy service: %w", err)
	}
	if len(cfg.mainConfigs) > 0 {
		svc.SetMainConfigs(cfg.mainConfigs)
	}
	if db != nil {
		if err := haproxy.EnsureHAProxyConfigSchema(ctx, db); err != nil {
			return nil, fmt.Errorf("init haproxy config schema: %w", err)
		}
		cs, err := haproxy.NewDBConfigStore(db)
		if err != nil {
			return nil, fmt.Errorf("init haproxy config store: %w", err)
		}
		svc.SetConfigStore(cs)
		// Seed: push any .cfg files already on disk into the DB so configs
		// created before DB_CONNECTION was configured survive future failovers.
		svc.SeedConfigStore(ctx)
		// Reconcile: write any DB configs missing from disk (post-failover).
		if err := svc.Reconcile(ctx); err != nil {
			log.Printf("warn: haproxy reconcile: %v", err)
		}
	}
	return svc, nil
}

/**
 * buildMySQLCluster assembles the MySQL cluster engine: a persistent job store,
 * an Ansible runner bound to the MySQL playbooks, and the service that ties them
 * together. Stale running jobs are marked failed on boot so a crash mid-deploy
 * never leaves a job stuck running. The store is returned as well as the service
 * because it is reused by the MySQL database manager.
 *
 * Params:
 *   ctx context.Context - base context for the service's background jobs.
 *   cfg clusterEngineConfig - state dir, playbook paths and Ansible tunables.
 *   ssh sshConfig - shared SSH credentials and host-key policy.
 *   proxyAllowedIP string - the HAProxy/control-plane IP allowed to reach
 *     client-facing DB ports and accounts (see PROXY_HOST).
 *   maxConcurrentJobs int - cap on concurrent background jobs for this engine.
 * Returns:
 *   mysqlcluster.Store - the job store (reused by the DB manager).
 *   *mysqlcluster.Service - the assembled cluster service.
 *   error - if the store cannot be created or SSH config is invalid.
 */
func buildMySQLCluster(ctx context.Context, cfg clusterEngineConfig, ssh sshConfig, proxyAllowedIP string, maxConcurrentJobs int, jobDB *sql.DB) (mysqlcluster.Store, *mysqlcluster.Service, error) {
	store, err := buildMySQLStore(cfg.stateDir, jobDB)
	if err != nil {
		return nil, nil, fmt.Errorf("init mysql cluster store: %w", err)
	}
	store.MarkStaleRunningJobsFailed()

	runner := mysqlcluster.NewRunner(cfg.ansible.bin, cfg.deployPlaybook, cfg.rollbackPlaybook)
	runner.SetAddMemberPlaybook(cfg.addMemberPlaybook)
	runner.SetRemoveMemberPlaybook(cfg.removeMemberPlaybook)
	runner.SetStopPlaybook(cfg.stopPlaybook)
	runner.SetDebug(cfg.ansible.verbosity, cfg.ansible.debug, cfg.ansible.stepOutputMaxChars)
	runner.SetSSHPolicy(ssh.policy())
	runner.SetProxyAllowedIP(proxyAllowedIP)

	svc := mysqlcluster.NewService(store, runner)
	svc.SetMaxConcurrentJobs(maxConcurrentJobs)
	svc.SetContext(ctx)
	if err := applySSHConfig(ssh, svc.SetSSHConfig); err != nil {
		return nil, nil, fmt.Errorf("init mysql ssh config: %w", err)
	}

	logAnsibleDebug("mysql", cfg.ansible)
	return store, svc, nil
}

/**
 * buildPGSQLCluster assembles the PostgreSQL cluster engine. It mirrors
 * buildMySQLCluster but its runner has no rollback playbook, because the
 * PostgreSQL deploy flow does not support rollback. The store is returned for
 * reuse by the PostgreSQL database manager.
 *
 * Params:
 *   ctx context.Context - base context for the service's background jobs.
 *   cfg clusterEngineConfig - state dir, playbook paths and Ansible tunables.
 *   ssh sshConfig - shared SSH credentials and host-key policy.
 *   controlPlane core.ControlPlane - the shared control plane carrying the
 *     Patroni DCS, when SHARED_CONTROL_PLANE is configured. PostgreSQL has no
 *     quorum mechanism of its own, so with it enabled new clusters keep no etcd
 *     on their own nodes and any node count is supported.
 *   proxyAllowedIP string - the HAProxy/control-plane IP allowed to reach
 *     client-facing DB ports and accounts (see PROXY_HOST).
 *   maxConcurrentJobs int - cap on concurrent background jobs for this engine.
 * Returns:
 *   pgsqlcluster.Store - the job store (reused by the DB manager).
 *   *pgsqlcluster.Service - the assembled cluster service.
 *   error - if the store cannot be created, SSH config is invalid, or the
 *     control plane is configured incompletely.
 */
func buildPGSQLCluster(ctx context.Context, cfg clusterEngineConfig, ssh sshConfig, controlPlane core.ControlPlane, proxyAllowedIP string, maxConcurrentJobs int, jobDB *sql.DB) (pgsqlcluster.Store, *pgsqlcluster.Service, error) {
	// Fail at start-up rather than mid-deploy: a control plane that is named but
	// not usable would otherwise produce clusters that cannot reach their DCS.
	if err := controlPlane.Validate(); err != nil {
		return nil, nil, fmt.Errorf("init pgsql control plane: %w", err)
	}

	store, err := buildPGSQLStore(cfg.stateDir, jobDB)
	if err != nil {
		return nil, nil, fmt.Errorf("init pgsql cluster store: %w", err)
	}
	store.MarkStaleRunningJobsFailed()

	runner := pgsqlcluster.NewRunner(cfg.ansible.bin, cfg.deployPlaybook)
	runner.SetAddMemberPlaybook(cfg.addMemberPlaybook)
	runner.SetRemoveMemberPlaybook(cfg.removeMemberPlaybook)
	runner.SetStopPlaybook(cfg.stopPlaybook)
	runner.SetDebug(cfg.ansible.verbosity, cfg.ansible.debug, cfg.ansible.stepOutputMaxChars)
	runner.SetSSHPolicy(ssh.policy())
	runner.SetProxyAllowedIP(proxyAllowedIP)
	runner.SetControlPlane(controlPlane)

	svc := pgsqlcluster.NewService(store, runner)
	svc.SetMaxConcurrentJobs(maxConcurrentJobs)
	svc.SetContext(ctx)
	if err := applySSHConfig(ssh, svc.SetSSHConfig); err != nil {
		return nil, nil, fmt.Errorf("init pgsql ssh config: %w", err)
	}

	logAnsibleDebug("pgsql", cfg.ansible)
	return store, svc, nil
}

func buildJobDB(ctx context.Context, conn string, pool dbPoolConfig) (*sql.DB, error) {
	if strings.TrimSpace(conn) == "" {
		return nil, nil
	}
	db, err := sql.Open("postgres", conn)
	if err != nil {
		return nil, fmt.Errorf("open job database: %w", err)
	}
	// Pool sizing — tune via DB_MAX_OPEN_CONNS / DB_MAX_IDLE_CONNS for the
	// host's CPU/RAM budget. Lifetime limits recycle connections so stale ones
	// are not held forever against a load-balanced PostgreSQL endpoint.
	db.SetMaxOpenConns(pool.maxOpenConns)
	db.SetMaxIdleConns(pool.maxIdleConns)
	db.SetConnMaxLifetime(pool.connMaxLifetime)
	db.SetConnMaxIdleTime(pool.connMaxIdleTime)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect job database: %w", err)
	}
	if err := core.EnsureJobStoreSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	log.Printf("job store database enabled (max_open=%d max_idle=%d lifetime=%s idle_time=%s)",
		pool.maxOpenConns, pool.maxIdleConns, pool.connMaxLifetime, pool.connMaxIdleTime)
	return db, nil
}

func buildMySQLStore(stateDir string, db *sql.DB) (mysqlcluster.Store, error) {
	fileStore, err := core.NewStore[mysqlcluster.StoredSpec, mysqlcluster.StoredSecret](stateDir)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return fileStore, nil
	}
	dbStore, err := mysqlcluster.NewDBStore(db)
	if err != nil {
		return nil, err
	}
	if err := fileStore.MoveJobsTo(dbStore); err != nil {
		return nil, err
	}
	return dbStore, nil
}

func buildPGSQLStore(stateDir string, db *sql.DB) (pgsqlcluster.Store, error) {
	fileStore, err := core.NewStore[pgsqlcluster.StoredSpec, pgsqlcluster.StoredSecret](stateDir)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return fileStore, nil
	}
	dbStore, err := pgsqlcluster.NewDBStore(db)
	if err != nil {
		return nil, err
	}
	if err := fileStore.MoveJobsTo(dbStore); err != nil {
		return nil, err
	}
	return dbStore, nil
}

/**
 * buildCipher constructs the AES-256-GCM cipher used by the encrypt/decrypt
 * middleware to protect request and response payloads. Encryption is optional:
 * when no key is configured the function returns a nil cipher and the middleware
 * degrades to a pass-through.
 *
 * Params:
 *   key string - hex-encoded 32-byte key, or "" to disable encryption.
 * Returns:
 *   *security.Cipher - the cipher, or nil when key is empty.
 *   error - if a non-empty key is invalid.
 */
func buildCipher(key string) (*security.Cipher, error) {
	if key == "" {
		return nil, nil
	}
	cipher, err := security.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init encryption cipher: %w", err)
	}
	log.Printf("payload encryption enabled (AES-256-GCM)")
	return cipher, nil
}

/**
 * applySSHConfig pushes the shared SSH credentials into a cluster service, but
 * only when credentials were actually provided. The setter differs per engine,
 * so it is passed in as a function; this keeps the "only set if present" guard
 * in one place instead of duplicated in every builder.
 *
 * Params:
 *   ssh sshConfig - the shared SSH credentials; ignored when none are present.
 *   set func(user, keyPath string) error - the engine's SSH-config setter.
 * Returns:
 *   error - whatever set returns, or nil when no credentials were provided.
 */
func applySSHConfig(ssh sshConfig, set func(user, keyPath string) error) error {
	if !ssh.hasCredentials() {
		return nil
	}
	return set(ssh.user, ssh.privateKeyPath)
}

/**
 * logAnsibleDebug emits a one-line summary of the Ansible debug settings for an
 * engine, but only when debugging is enabled — so normal runs stay quiet.
 *
 * Params:
 *   engine string - engine label for the log line, e.g. "mysql".
 *   cfg ansibleConfig - the Ansible tunables; nothing is logged unless debug.
 */
func logAnsibleDebug(engine string, cfg ansibleConfig) {
	if !cfg.debug {
		return
	}
	log.Printf("%s ansible debug enabled: verbosity=%d, step_output_max_chars=%d",
		engine, cfg.verbosity, cfg.stepOutputMaxChars)
}
