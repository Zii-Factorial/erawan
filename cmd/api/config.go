package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"erawan-cluster/internal/cluster/core"
	"erawan-cluster/internal/env"
)

// config holds the small set of process-wide settings that HTTP handlers need
// at request time: the listen address, environment name, API key for auth, the
// build version, and the HAProxy host used when dialing cluster metrics.
//
// It is deliberately kept narrow. Anything only needed once during start-up
// (playbook paths, state directories, Ansible tunables) lives in the *Config
// structs below and is consumed by the builders in setup.go, never carried on
// the long-lived application value.
type config struct {
	addr      string
	env       string
	apiKey    string
	version   string
	proxyHost string // HAProxy host for metric connections; from PROXY_HOST env (default 127.0.0.1)
}

// dbPoolConfig holds PostgreSQL connection-pool tunables. Sized for vertical
// scaling: raise DB_MAX_OPEN_CONNS when adding CPU/RAM to the host.
type dbPoolConfig struct {
	maxOpenConns    int
	maxIdleConns    int
	connMaxLifetime time.Duration
	connMaxIdleTime time.Duration
}

// runtimeConfig is the fully-resolved configuration for the whole API process.
// It is loaded exactly once, at start-up, by loadConfig and then handed to the
// builders in setup.go. Grouping every tunable here (rather than reading the
// environment ad hoc throughout main) keeps configuration in one auditable
// place and makes adding a new engine a matter of adding one more field.
type runtimeConfig struct {
	server               config
	haproxy              haproxyConfig
	ssh                  sshConfig
	controlPlane         core.ControlPlane
	mysql                clusterEngineConfig
	pgsql                clusterEngineConfig
	encryptionKey        string
	dbConnection         string
	dbPool               dbPoolConfig
	baseDir              string
	maxConcurrentJobs    int  // cap on concurrent background cluster jobs (ansible runs)
	shutdownDrainSeconds int  // how long to wait for in-flight jobs to finish on SIGTERM
	enablePprof          bool // expose /debug/pprof on the loopback interface
}

// haproxyConfig describes how the HAProxy service writes tenant fragments and
// reloads the proxy. tenantsDir is where per-tenant config files are written,
// reloadCmd is the argv used to reload HAProxy, reloadTimeout bounds that
// command, and mainConfigs lists the operator-managed base config files that
// must never be deleted by tenant operations.
type haproxyConfig struct {
	tenantsDir    string
	reloadCmd     []string
	reloadTimeout time.Duration
	mainConfigs   []string
}

// sshConfig holds the SSH credentials Ansible uses to reach cluster nodes. The
// same credentials are shared by every engine. When both fields are empty the
// runner falls back to the ambient SSH configuration of the host.
type sshConfig struct {
	user           string
	privateKeyPath string
	verifyHostKeys bool
	knownHostsFile string
}

/**
 * policy maps the resolved SSH settings to the engine-agnostic host-key policy
 * used by the Ansible runners.
 *
 * Receiver:
 *   s sshConfig - the resolved SSH settings (by value).
 * Returns:
 *   core.SSHPolicy - host-key verification flag plus optional known_hosts path.
 */
func (s sshConfig) policy() core.SSHPolicy {
	return core.SSHPolicy{VerifyHostKeys: s.verifyHostKeys, KnownHostsFile: s.knownHostsFile}
}

/**
 * hasCredentials reports whether any explicit SSH credential was provided and
 * therefore should be pushed into the cluster services. When false, the
 * services keep their default (ambient) SSH behaviour.
 *
 * Receiver:
 *   s sshConfig - the resolved SSH settings (by value).
 * Returns:
 *   bool - true if a user or private-key path was configured.
 */
func (s sshConfig) hasCredentials() bool {
	return strings.TrimSpace(s.user) != "" || strings.TrimSpace(s.privateKeyPath) != ""
}

// ansibleConfig captures the Ansible execution tunables for a single engine:
// which playbook binary to invoke and how verbose / how much captured output
// to retain when debugging a deploy.
type ansibleConfig struct {
	bin                string
	debug              bool
	verbosity          int
	stepOutputMaxChars int
}

// clusterEngineConfig is the per-engine configuration for a SQL cluster engine
// (currently MySQL and PostgreSQL). Every engine is provisioned the same way —
// a job state directory plus a set of Ansible playbooks — so a new engine only
// needs to populate one of these and call the matching builder in setup.go.
//
// rollbackPlaybook is engine-specific: MySQL supports rollback, PostgreSQL does
// not, so it is left empty for engines without that capability.
type clusterEngineConfig struct {
	stateDir             string
	deployPlaybook       string
	addMemberPlaybook    string
	removeMemberPlaybook string
	stopPlaybook         string
	rollbackPlaybook     string
	ansible              ansibleConfig
}

/**
 * loadConfig reads the entire process configuration from the environment,
 * applying sane defaults so the service can run unconfigured in development.
 * It performs no I/O beyond resolving the working directory and never fails;
 * validation of the resolved values happens in the builders.
 *
 * Returns:
 *   runtimeConfig - the fully-resolved configuration for the whole process.
 */
func loadConfig() runtimeConfig {
	baseDir := projectBaseDir()
	ansibleBin := env.GetString("ANSIBLE_PLAYBOOK_BIN", "ansible-playbook")

	// All engines store their job history under a shared root by default, each
	// in its own sub-directory; an engine may override with its own *_STATE_DIR.
	sharedStateDir := env.GetString("CLUSTER_STATE_DIR", "/var/lib/erawan-cluster/cluster/jobs")

	mysqlCfg := loadClusterEngineConfig("mysql", baseDir, sharedStateDir, ansibleBin)
	mysqlCfg.rollbackPlaybook = env.GetString(
		"MYSQL_ROLLBACK_PLAYBOOK",
		filepath.Join(baseDir, "cluster/mysql/playbooks/rollback.yml"),
	)

	pgsqlCfg := loadClusterEngineConfig("pgsql", baseDir, sharedStateDir, ansibleBin)

	return runtimeConfig{
		server:            loadServerConfig(),
		haproxy:           loadHAProxyConfig(),
		ssh:               loadSSHConfig(),
		controlPlane:      loadControlPlaneConfig(baseDir),
		mysql:             mysqlCfg,
		pgsql:             pgsqlCfg,
		encryptionKey:     env.GetString("ENCRYPTION_KEY", ""),
		dbConnection:      env.GetString("DB_CONNECTION", ""),
		dbPool:            loadDBPoolConfig(),
		baseDir:           baseDir,
		maxConcurrentJobs: env.GetInt("CLUSTER_MAX_CONCURRENT_JOBS", 4),
		// Allow up to 5 minutes for in-flight Ansible jobs to write their final
		// status before the process exits. Raise this if deploy steps exceed 5 min.
		shutdownDrainSeconds: env.GetInt("SHUTDOWN_DRAIN_SECONDS", 300),
		enablePprof:          env.GetBool("ENABLE_PPROF", false),
	}
}

/**
 * loadServerConfig resolves the HTTP server and request-time settings. The
 * listen address may be given directly via API_ADDR, or assembled from
 * API_HOST and API_PORT when API_ADDR is unset/blank.
 *
 * Returns:
 *   config - listen address, env name, API key, version and proxy host.
 */
func loadServerConfig() config {
	addr := env.GetString("API_ADDR", "")
	if strings.TrimSpace(addr) == "" {
		host := env.GetString("API_HOST", "0.0.0.0")
		port := env.GetString("API_PORT", "8080")
		addr = host + ":" + port
	}

	return config{
		addr:      addr,
		env:       env.GetString("ENV", "dev"),
		apiKey:    env.GetString("API_KEY", ""),
		version:   appVersion,
		proxyHost: env.GetString("PROXY_HOST", "127.0.0.1"),
	}
}

/**
 * loadHAProxyConfig resolves where tenant fragments live and how the proxy is
 * reloaded. HAPROXY_MAIN_CONFIGS is an optional comma-separated list of base
 * config files that tenant operations must leave untouched.
 *
 * Returns:
 *   haproxyConfig - tenants dir, reload command/timeout, and main config list.
 */
func loadHAProxyConfig() haproxyConfig {
	var mainConfigs []string
	if raw := env.GetString("HAPROXY_MAIN_CONFIGS", ""); raw != "" {
		mainConfigs = strings.Split(raw, ",")
	}

	return haproxyConfig{
		tenantsDir:    env.GetString("TENANTS_DIR", "/var/lib/erawan-cluster/haproxy/tenants"),
		reloadCmd:     parseCommand(env.GetString("HAPROXY_RELOAD_CMD", "sudo /bin/systemctl reload haproxy")),
		reloadTimeout: time.Duration(env.GetInt("HAPROXY_RELOAD_TIMEOUT_SECONDS", 15)) * time.Second,
		mainConfigs:   mainConfigs,
	}
}

/**
 * loadSSHConfig resolves the shared SSH credentials and host-key policy used by
 * every cluster engine's Ansible runner. Host-key verification is on by default
 * and only disabled when CLUSTER_SSH_INSECURE_HOST_KEY is set.
 *
 * Returns:
 *   sshConfig - SSH user, private-key path, verify-host-keys flag and
 *     known_hosts path.
 */
func loadSSHConfig() sshConfig {
	// Secure by default: verify node SSH host keys unless explicitly disabled
	// (e.g. for greenfield bootstrap) via CLUSTER_SSH_INSECURE_HOST_KEY=true.
	insecure := env.GetBool("CLUSTER_SSH_INSECURE_HOST_KEY", false)
	return sshConfig{
		user:           env.GetString("CLUSTER_SSH_USER", ""),
		privateKeyPath: env.GetString("CLUSTER_SSH_PRIVATE_KEY_PATH", ""),
		verifyHostKeys: !insecure,
		knownHostsFile: env.GetString("CLUSTER_SSH_KNOWN_HOSTS", ""),
	}
}

/**
 * loadControlPlaneConfig resolves the shared control plane that carries the DCS
 * for engines without a quorum mechanism of their own (PostgreSQL/Patroni).
 *
 * SHARED_CONTROL_PLANE is the switch: unset (the default) keeps every engine on
 * its classic per-node DCS, so this is opt-in and existing deployments are
 * unaffected. The remaining keys only matter once it is set, and every one of
 * them has a working default except the root password — which has none on
 * purpose, so a half-configured control plane is caught at start-up rather than
 * halfway through a deploy (see core.ControlPlane.Validate).
 *
 * Params:
 *   baseDir string - project base directory the default playbook paths hang off.
 * Returns:
 *   core.ControlPlane - the resolved control plane; disabled when the IP is unset.
 */
func loadControlPlaneConfig(baseDir string) core.ControlPlane {
	sharedPlaybookDir := filepath.Join(baseDir, "cluster", "shared", "playbooks")
	return core.ControlPlane{
		// SHARED_CONTROL_PLANE is the documented name; the two variants are
		// accepted so an .env written against an earlier spelling keeps working.
		IP: strings.TrimSpace(env.GetStringAny(
			[]string{"SHARED_CONTROL_PLANE", "SHARED_CONTROL_PLAN", "SHAER_CONTROL_PLAN"}, "",
		)),
		EtcdClientPort: env.GetInt("CONTROL_PLANE_ETCD_CLIENT_PORT", 2379),
		CACertPath:     env.GetString("CONTROL_PLANE_ETCD_CACERT", "/etc/etcd/ssl/ca.pem"),
		CertPath:       env.GetString("CONTROL_PLANE_ETCD_CERT", "/etc/etcd/ssl/cp-etcd-01.pem"),
		KeyPath:        env.GetString("CONTROL_PLANE_ETCD_KEY", "/etc/etcd/ssl/cp-etcd-01-key.pem"),
		RootUser:       env.GetString("CONTROL_PLANE_ETCD_ROOT_USER", "root"),
		RootPassword:   env.GetString("CONTROL_PLANE_ETCD_ROOT_PASSWORD", ""),
		// Patroni's key path is namespace + scope, so a tenant's keys land in
		// /db/patroni/<cluster>/ and its etcd role is granted exactly that.
		Namespace:        env.GetString("CONTROL_PLANE_DCS_NAMESPACE", "/db/patroni/"),
		TenantNamePrefix: env.GetString("CONTROL_PLANE_DCS_TENANT_PREFIX", "patroni"),
		NodeCAPath:       env.GetString("CONTROL_PLANE_ETCD_NODE_CA_PATH", "/etc/patroni/etcd-ca.pem"),
		NodeCAOwner:      env.GetString("CONTROL_PLANE_ETCD_NODE_CA_OWNER", "postgres"),
		// The control plane is built from the same node template, so it is
		// normally reached with the same credentials as the DB nodes; these
		// override that per field when it is not.
		SSHUser:           env.GetString("CONTROL_PLANE_SSH_USER", ""),
		SSHPrivateKeyPath: env.GetString("CONTROL_PLANE_SSH_PRIVATE_KEY_PATH", ""),
		SSHPort:           env.GetInt("CONTROL_PLANE_SSH_PORT", 0),
		ProvisionPlaybook: env.GetString(
			"CONTROL_PLANE_DCS_PROVISION_PLAYBOOK",
			filepath.Join(sharedPlaybookDir, "control_plane_dcs_provision.yml"),
		),
		CleanupPlaybook: env.GetString(
			"CONTROL_PLANE_DCS_CLEANUP_PLAYBOOK",
			filepath.Join(sharedPlaybookDir, "control_plane_dcs_cleanup.yml"),
		),
	}
}

/**
 * loadClusterEngineConfig resolves the playbook paths and state directory for
 * one cluster engine. The name drives both the default on-disk layout
 * (cluster/<name>/playbooks/...) and the uppercase environment-variable prefix
 * (<NAME>_DEPLOY_PLAYBOOK, ...), so wiring a new engine is purely declarative.
 *
 * Params:
 *   name string - lowercase engine name, e.g. "mysql"; drives paths and the
 *     env-var prefix.
 *   baseDir string - project base directory the default playbook paths hang off.
 *   sharedStateDir string - default parent for the engine's job state dir.
 *   ansibleBin string - the ansible-playbook binary to invoke.
 * Returns:
 *   clusterEngineConfig - state dir, playbook paths and Ansible tunables.
 */
func loadClusterEngineConfig(name, baseDir, sharedStateDir, ansibleBin string) clusterEngineConfig {
	prefix := strings.ToUpper(name)
	playbookDir := filepath.Join(baseDir, "cluster", name, "playbooks")

	return clusterEngineConfig{
		stateDir:             env.GetString(prefix+"_CLUSTER_STATE_DIR", filepath.Join(sharedStateDir, name)),
		deployPlaybook:       env.GetString(prefix+"_DEPLOY_PLAYBOOK", filepath.Join(playbookDir, "deploy.yml")),
		addMemberPlaybook:    env.GetString(prefix+"_ADD_MEMBER_PLAYBOOK", filepath.Join(playbookDir, "add_member.yml")),
		removeMemberPlaybook: env.GetString(prefix+"_REMOVE_MEMBER_PLAYBOOK", filepath.Join(playbookDir, "remove_member.yml")),
		stopPlaybook:         env.GetString(prefix+"_STOP_PLAYBOOK", filepath.Join(playbookDir, "stop.yml")),
		ansible:              loadAnsibleConfig(prefix, ansibleBin),
	}
}

/**
 * loadAnsibleConfig resolves the Ansible execution tunables for one engine.
 * Each value may be set globally (CLUSTER_*) or per engine (<PREFIX>_*); the
 * global key is checked first, so it takes precedence when both are present.
 * Turning debugging on without explicit values bumps verbosity and the
 * captured-output cap to diagnostic-friendly defaults.
 *
 * Params:
 *   prefix string - uppercase engine prefix, e.g. "MYSQL", for env-var lookups.
 *   bin string - the ansible-playbook binary to record in the config.
 * Returns:
 *   ansibleConfig - binary, debug flag, verbosity and step-output cap.
 */
func loadAnsibleConfig(prefix, bin string) ansibleConfig {
	debug := env.GetBoolAny([]string{"CLUSTER_ANSIBLE_DEBUG", prefix + "_ANSIBLE_DEBUG"}, false)
	verbosity := env.GetIntAny([]string{"CLUSTER_ANSIBLE_VERBOSITY", prefix + "_ANSIBLE_VERBOSITY"}, 0)
	stepOutputMaxChars := env.GetIntAny([]string{"CLUSTER_STEP_OUTPUT_MAX_CHARS", prefix + "_STEP_OUTPUT_MAX_CHARS"}, 8000)

	if debug && verbosity <= 0 {
		verbosity = 3
	}
	if debug && stepOutputMaxChars == 8000 {
		stepOutputMaxChars = 200000
	}

	return ansibleConfig{
		bin:                bin,
		debug:              debug,
		verbosity:          verbosity,
		stepOutputMaxChars: stepOutputMaxChars,
	}
}

/**
 * parseCommand splits a shell-style command string into an argv slice. If the
 * input is empty it falls back to the default HAProxy reload command so the
 * service always has something runnable.
 *
 * Params:
 *   raw string - the space-separated command string to split.
 * Returns:
 *   []string - the argv; the default reload command when raw is empty.
 */
func parseCommand(raw string) []string {
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		return []string{"sudo", "/bin/systemctl", "reload", "haproxy"}
	}
	return parts
}

/**
 * projectBaseDir returns the directory the process was started from, used as the
 * root for resolving default playbook and asset paths. It falls back to "." when
 * the working directory cannot be determined.
 *
 * Returns:
 *   string - the current working directory, or "." on error.
 */
func projectBaseDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// loadDBPoolConfig resolves PostgreSQL connection-pool tunables from the
// environment. Defaults are conservative; raise them proportionally when
// scaling the host vertically (more CPU/RAM = more concurrent DB operations).
//
// Recommended starting points:
//
//	DB_MAX_OPEN_CONNS  = (num_cpu * 2) + headroom, e.g. 25 for 8-core
//	DB_MAX_IDLE_CONNS  = DB_MAX_OPEN_CONNS / 2
func loadDBPoolConfig() dbPoolConfig {
	return dbPoolConfig{
		maxOpenConns:    env.GetInt("DB_MAX_OPEN_CONNS", 25),
		maxIdleConns:    env.GetInt("DB_MAX_IDLE_CONNS", 10),
		connMaxLifetime: time.Duration(env.GetInt("DB_CONN_MAX_LIFETIME_SECONDS", 300)) * time.Second,
		connMaxIdleTime: time.Duration(env.GetInt("DB_CONN_MAX_IDLE_TIME_SECONDS", 60)) * time.Second,
	}
}
