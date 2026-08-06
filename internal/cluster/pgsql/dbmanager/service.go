package dbmanager

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	pgsql "erawan-cluster/internal/cluster/pgsql"
	"github.com/lib/pq"
)

type Service struct {
	store          pgsql.Store
	httpClient     *http.Client
	proxyAllowedIP string
}

/**
 * NewService.
 *
 * Params:
 *   store pgsql.Store - the job store.
 *   proxyAllowedIP string - the HAProxy/control-plane IP that app users
 *     created/managed through this service are scoped to in pg_hba (see
 *     PROXY_HOST).
 *
 * Returns:
 *   *Service - the resulting *Service
 */
func NewService(store pgsql.Store, proxyAllowedIP string) *Service {
	return &Service{
		store:          store,
		httpClient:     &http.Client{Timeout: 4 * time.Second},
		proxyAllowedIP: proxyAllowedIP,
	}
}

/**
 * resolve loads the current primary IP, port, and postgres superuser credentials.
 * It probes each node's Patroni /master endpoint so the result is correct even
 * after a failover since the original deploy.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   jobID string - the jobID string
 *
 * Returns:
 *   host string - the host string
 *   port int - the port value
 *   user string - the user string
 *   password string - the password string
 *   err error - error value; non-nil when the operation fails
 */
func (s *Service) resolve(ctx context.Context, jobID string) (host string, port int, user, password string, err error) {
	job, err := s.store.Load(jobID)
	if err != nil {
		return "", 0, "", "", fmt.Errorf("load job %q: %w", jobID, err)
	}
	secret, err := s.store.LoadSecret(jobID)
	if err != nil {
		return "", 0, "", "", fmt.Errorf("load job secret %q: %w", jobID, err)
	}
	p := job.Request.PostgresPort
	if p == 0 {
		p = 5432
	}
	candidates := append([]string{job.Request.PrimaryIP}, job.Request.StandbyIPs...)
	primary, err := s.findPrimary(ctx, candidates)
	if err != nil {
		return "", 0, "", "", fmt.Errorf("discover primary: %w", err)
	}
	return primary, p, secret.PostgresUser, secret.PostgresPassword, nil
}

/**
 * findPrimary probes each node's Patroni /master endpoint (port 8008) and
 * returns the first node that responds 200 — that is the current read-write leader.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   candidates []string - the candidates ([]string)
 *
 * Returns:
 *   string - the resulting string
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) findPrimary(ctx context.Context, candidates []string) (string, error) {
	for _, ip := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("http://%s:8008/master", ip), nil)
		if err != nil {
			continue
		}
		resp, err := s.httpClient.Do(req)
		if err != nil {
			continue
		}
		status := resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if status == http.StatusOK {
			return ip, nil
		}
	}
	return "", fmt.Errorf("no primary found among nodes %v", candidates)
}

/**
 * CreateUser.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   req CreateUserRequest - the req (CreateUserRequest)
 *
 * Returns:
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) CreateUser(ctx context.Context, req CreateUserRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	host, port, adminUser, adminPass, err := s.resolve(ctx, req.JobID)
	if err != nil {
		return err
	}

	root, err := s.connect(ctx, host, port, "postgres", adminUser, adminPass)
	if err != nil {
		return err
	}
	defer root.Close()

	userLit := pq.QuoteLiteral(req.Username)
	passLit := pq.QuoteLiteral(req.Password)
	uid := pq.QuoteIdentifier(req.Username)

	if req.Superuser {
		const attrs = "LOGIN SUPERUSER CREATEDB CREATEROLE REPLICATION BYPASSRLS"
		upsertRole := fmt.Sprintf(`
			DO $body$
			BEGIN
			  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
			    EXECUTE format('CREATE ROLE %%I WITH %s PASSWORD %%L', %s, %s);
			  ELSE
			    EXECUTE format('ALTER ROLE %%I WITH %s PASSWORD %%L', %s, %s);
			  END IF;
			END
			$body$`, userLit, attrs, userLit, passLit, attrs, userLit, passLit)
		if _, err := root.ExecContext(ctx, upsertRole); err != nil {
			return fmt.Errorf("upsert role: %w", err)
		}
	} else {
		const nonSuperAttrs = "LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"
		upsertRole := fmt.Sprintf(`
			DO $body$
			BEGIN
			  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
			    EXECUTE format(
			      'CREATE ROLE %%I WITH %s PASSWORD %%L',
			      %s, %s);
			  ELSE
			    EXECUTE format(
			      'ALTER ROLE %%I WITH %s PASSWORD %%L',
			      %s, %s);
			  END IF;
			END
			$body$`, userLit, nonSuperAttrs, userLit, passLit, nonSuperAttrs, userLit, passLit)
		if _, err := root.ExecContext(ctx, upsertRole); err != nil {
			return fmt.Errorf("upsert role: %w", err)
		}

		if req.DatabaseName != "" {
			existingUsers, err := appUsers(ctx, root)
			if err != nil {
				return fmt.Errorf("list users: %w", err)
			}
			peers := without(existingUsers, req.Username)

			var dbExists bool
			if err := root.QueryRowContext(ctx,
				"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", req.DatabaseName,
			).Scan(&dbExists); err != nil {
				return fmt.Errorf("check database: %w", err)
			}
			if !dbExists {
				if _, err := root.ExecContext(ctx,
					"CREATE DATABASE "+pq.QuoteIdentifier(req.DatabaseName)+" OWNER "+pq.QuoteIdentifier(adminUser),
				); err != nil {
					return fmt.Errorf("create database: %w", err)
				}
			}
			if _, err := root.ExecContext(ctx,
				"GRANT ALL PRIVILEGES ON DATABASE "+pq.QuoteIdentifier(req.DatabaseName)+" TO "+uid,
			); err != nil {
				return fmt.Errorf("grant on database %q: %w", req.DatabaseName, err)
			}
			if err := s.grantInDatabase(ctx, host, port, req.DatabaseName, adminUser, adminPass, req.Username, peers); err != nil {
				return err
			}
		}
	}

	if err := s.updatePatroniPgHba(ctx, req.JobID, req.Username, req.SSLRequiredEnabled()); err != nil {
		return fmt.Errorf("update pg_hba: %w", err)
	}
	return nil
}

/**
 * ResetPassword.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   req ResetPasswordRequest - the req (ResetPasswordRequest)
 *
 * Returns:
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) ResetPassword(ctx context.Context, req ResetPasswordRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	host, port, adminUser, adminPass, err := s.resolve(ctx, req.JobID)
	if err != nil {
		return err
	}

	root, err := s.connect(ctx, host, port, "postgres", adminUser, adminPass)
	if err != nil {
		return err
	}
	defer root.Close()

	var rolName string
	err = root.QueryRowContext(ctx,
		`SELECT rolname FROM pg_roles WHERE rolname = $1`,
		req.Username,
	).Scan(&rolName)
	if err == sql.ErrNoRows {
		return fmt.Errorf("user %q does not exist", req.Username)
	}
	if err != nil {
		return fmt.Errorf("lookup role: %w", err)
	}
	if rolName == "postgres" || strings.HasPrefix(rolName, "pg_") {
		return fmt.Errorf("user %q is a protected system role and cannot be modified", req.Username)
	}

	if _, err := root.ExecContext(ctx,
		"ALTER ROLE "+pq.QuoteIdentifier(req.Username)+" WITH PASSWORD "+pq.QuoteLiteral(req.Password),
	); err != nil {
		return fmt.Errorf("reset password: %w", err)
	}
	return nil
}

/**
 * UpdateUser.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   req UpdateUserRequest - the req (UpdateUserRequest)
 *
 * Returns:
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) UpdateUser(ctx context.Context, req UpdateUserRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	host, port, adminUser, adminPass, err := s.resolve(ctx, req.JobID)
	if err != nil {
		return err
	}
	secret, err := s.store.LoadSecret(req.JobID)
	if err != nil {
		return fmt.Errorf("load secret: %w", err)
	}

	root, err := s.connect(ctx, host, port, "postgres", adminUser, adminPass)
	if err != nil {
		return err
	}
	defer root.Close()

	var rolName string
	err = root.QueryRowContext(ctx,
		`SELECT rolname FROM pg_roles WHERE rolname = $1`,
		req.Username,
	).Scan(&rolName)
	if err == sql.ErrNoRows {
		return fmt.Errorf("user %q does not exist", req.Username)
	}
	if err != nil {
		return fmt.Errorf("lookup role: %w", err)
	}
	if rolName == "postgres" || strings.HasPrefix(rolName, "pg_") {
		return fmt.Errorf("user %q is a protected system role and cannot be renamed", req.Username)
	}
	if req.Username == secret.ReplicatorUser {
		return fmt.Errorf("user %q is the replication user and cannot be renamed through this API", req.Username)
	}
	if req.Username == adminUser {
		return fmt.Errorf("user %q is the cluster admin and cannot be renamed through this API", req.Username)
	}

	if _, err := root.ExecContext(ctx,
		"ALTER ROLE "+pq.QuoteIdentifier(req.Username)+" RENAME TO "+pq.QuoteIdentifier(req.NewUsername),
	); err != nil {
		return fmt.Errorf("rename role: %w", err)
	}
	return nil
}

/**
 * DeleteUser.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   req DeleteUserRequest - the req (DeleteUserRequest)
 *
 * Returns:
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) DeleteUser(ctx context.Context, req DeleteUserRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	host, port, adminUser, adminPass, err := s.resolve(ctx, req.JobID)
	if err != nil {
		return err
	}
	secret, err := s.store.LoadSecret(req.JobID)
	if err != nil {
		return fmt.Errorf("load secret: %w", err)
	}

	root, err := s.connect(ctx, host, port, "postgres", adminUser, adminPass)
	if err != nil {
		return err
	}
	defer root.Close()

	var rolName string
	err = root.QueryRowContext(ctx,
		`SELECT rolname FROM pg_roles WHERE rolname = $1`,
		req.Username,
	).Scan(&rolName)
	if err == sql.ErrNoRows {
		return fmt.Errorf("user %q does not exist", req.Username)
	}
	if err != nil {
		return fmt.Errorf("lookup role: %w", err)
	}
	if rolName == "postgres" || strings.HasPrefix(rolName, "pg_") {
		return fmt.Errorf("user %q is a protected system role and cannot be deleted", req.Username)
	}
	if req.Username == secret.ReplicatorUser {
		return fmt.Errorf("user %q is the replication user and cannot be deleted through this API", req.Username)
	}
	if req.Username == adminUser {
		return fmt.Errorf("user %q is the cluster admin and cannot be deleted through this API", req.Username)
	}

	dbs, err := appDatabases(ctx, root)
	if err != nil {
		return fmt.Errorf("list databases: %w", err)
	}
	for _, dbname := range dbs {
		if err := s.revokeInDatabase(ctx, host, port, dbname, adminUser, adminPass, req.Username); err != nil {
			return err
		}
	}

	if _, err := root.ExecContext(ctx, "DROP ROLE "+pq.QuoteIdentifier(req.Username)); err != nil {
		return fmt.Errorf("drop role: %w", err)
	}
	// best-effort cleanup of any pg_hba entries for this user
	_ = s.removeFromPatroniPgHba(ctx, req.JobID, req.Username)
	return nil
}

/**
 * CreateDatabase.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   req CreateDatabaseRequest - the req (CreateDatabaseRequest)
 *
 * Returns:
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) CreateDatabase(ctx context.Context, req CreateDatabaseRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	host, port, adminUser, adminPass, err := s.resolve(ctx, req.JobID)
	if err != nil {
		return err
	}

	root, err := s.connect(ctx, host, port, "postgres", adminUser, adminPass)
	if err != nil {
		return err
	}
	defer root.Close()

	dbid := pq.QuoteIdentifier(req.DBName)
	if _, err := root.ExecContext(ctx,
		"CREATE DATABASE "+dbid+" OWNER "+pq.QuoteIdentifier(adminUser),
	); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	return nil
}

/**
 * UpdateDatabase.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   req UpdateDatabaseRequest - the req (UpdateDatabaseRequest)
 *
 * Returns:
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) UpdateDatabase(ctx context.Context, req UpdateDatabaseRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	switch req.DBName {
	case "postgres", "template0", "template1":
		return fmt.Errorf("database %q is a system database and cannot be renamed", req.DBName)
	}

	host, port, adminUser, adminPass, err := s.resolve(ctx, req.JobID)
	if err != nil {
		return err
	}

	root, err := s.connect(ctx, host, port, "postgres", adminUser, adminPass)
	if err != nil {
		return err
	}
	defer root.Close()

	if _, err := root.ExecContext(ctx,
		`SELECT pg_terminate_backend(pid)
		   FROM pg_stat_activity
		  WHERE datname = $1 AND pid <> pg_backend_pid()`,
		req.DBName,
	); err != nil {
		return fmt.Errorf("terminate connections: %w", err)
	}

	if _, err := root.ExecContext(ctx,
		"ALTER DATABASE "+pq.QuoteIdentifier(req.DBName)+" RENAME TO "+pq.QuoteIdentifier(req.NewDBName),
	); err != nil {
		return fmt.Errorf("rename database: %w", err)
	}
	return nil
}

/**
 * DeleteDatabase.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   req DeleteDatabaseRequest - the req (DeleteDatabaseRequest)
 *
 * Returns:
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) DeleteDatabase(ctx context.Context, req DeleteDatabaseRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	switch req.DBName {
	case "postgres", "template0", "template1":
		return fmt.Errorf("database %q is a system database and cannot be deleted", req.DBName)
	}

	host, port, adminUser, adminPass, err := s.resolve(ctx, req.JobID)
	if err != nil {
		return err
	}

	root, err := s.connect(ctx, host, port, "postgres", adminUser, adminPass)
	if err != nil {
		return err
	}
	defer root.Close()

	if _, err := root.ExecContext(ctx,
		`SELECT pg_terminate_backend(pid)
		   FROM pg_stat_activity
		  WHERE datname = $1 AND pid <> pg_backend_pid()`,
		req.DBName,
	); err != nil {
		return fmt.Errorf("terminate connections: %w", err)
	}

	if _, err := root.ExecContext(ctx, "DROP DATABASE "+pq.QuoteIdentifier(req.DBName)); err != nil {
		return fmt.Errorf("drop database: %w", err)
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

/**
 * connect.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   host string - the host string
 *   port int - the port value
 *   dbname string - the dbname string
 *   user string - the user string
 *   password string - the password string
 *
 * Returns:
 *   *sql.DB - the resulting *sql.DB
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) connect(ctx context.Context, host string, port int, dbname, user, password string) (*sql.DB, error) {
	// Secure by default: verify-full validates the server certificate and host
	// name (anti-MITM). Relax via CLUSTER_DB_SSL_MODE for self-signed clusters.
	dsn := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s",
		host, port, pgConnVal(dbname), pgConnVal(user), pgConnVal(password), adminSSLMode())
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect %s:%d/%s: %w", host, port, dbname, err)
	}
	return db, nil
}

/**
 * adminSSLMode resolves the lib/pq sslmode for admin connections. Defaults to
 * "verify-full"; operators may relax it via CLUSTER_DB_SSL_MODE (e.g. require,
 * verify-ca, disable) for clusters using self-signed certificates.
 *
 * Returns:
 *   string - the resulting string
 */
func adminSSLMode() string {
	switch m := strings.ToLower(strings.TrimSpace(os.Getenv("CLUSTER_DB_SSL_MODE"))); m {
	case "disable", "require", "verify-ca", "verify-full", "prefer", "allow":
		return m
	default:
		return "require"
	}
}

/**
 * pgConnVal.
 *
 * Params:
 *   v string - the v string
 *
 * Returns:
 *   string - the resulting string
 */
func pgConnVal(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}

/**
 * appDatabases.
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   db *sql.DB - the db (*sql.DB)
 *
 * Returns:
 *   []string - the resulting []string
 *   error - error value; non-nil when the operation fails
 */
func appDatabases(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT datname FROM pg_database
		  WHERE datistemplate = false AND datname <> 'postgres'
		  ORDER BY datname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

/**
 * appUsers.
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   db *sql.DB - the db (*sql.DB)
 *
 * Returns:
 *   []string - the resulting []string
 *   error - error value; non-nil when the operation fails
 */
func appUsers(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT rolname FROM pg_roles
		  WHERE rolcanlogin
		    AND NOT rolsuper
		    AND NOT rolreplication
		    AND rolname NOT LIKE 'pg_%'
		    AND rolname <> 'postgres'
		  ORDER BY rolname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

/**
 * grantInDatabase.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   host string - the host string
 *   port int - the port value
 *   dbname string - the dbname string
 *   adminUser string - the adminUser string
 *   adminPass string - the adminPass string
 *   targetUser string - the targetUser string
 *   peers []string - the peers ([]string)
 *
 * Returns:
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) grantInDatabase(ctx context.Context, host string, port int,
	dbname, adminUser, adminPass, targetUser string, peers []string) error {

	db, err := s.connect(ctx, host, port, dbname, adminUser, adminPass)
	if err != nil {
		return err
	}
	defer db.Close()

	target := pq.QuoteIdentifier(targetUser)
	for _, stmt := range []string{
		"GRANT USAGE ON SCHEMA public TO " + target,
		"GRANT CREATE ON SCHEMA public TO " + target,
		"GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public TO " + target,
		"GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO " + target,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLES TO " + target,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO " + target,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("db %q: %s: %w", dbname, stmt, err)
		}
	}
	for _, peer := range peers {
		pid := pq.QuoteIdentifier(peer)
		_, _ = db.ExecContext(ctx, fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLES TO %s", pid, target))
		_, _ = db.ExecContext(ctx, fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT ALL ON SEQUENCES TO %s", pid, target))
		_, _ = db.ExecContext(ctx, fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLES TO %s", target, pid))
		_, _ = db.ExecContext(ctx, fmt.Sprintf("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT ALL ON SEQUENCES TO %s", target, pid))
	}
	return nil
}

/**
 * revokeInDatabase.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   host string - the host string
 *   port int - the port value
 *   dbname string - the dbname string
 *   adminUser string - the adminUser string
 *   adminPass string - the adminPass string
 *   targetUser string - the targetUser string
 *
 * Returns:
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) revokeInDatabase(ctx context.Context, host string, port int,
	dbname, adminUser, adminPass, targetUser string) error {

	db, err := s.connect(ctx, host, port, dbname, adminUser, adminPass)
	if err != nil {
		return err
	}
	defer db.Close()

	target := pq.QuoteIdentifier(targetUser)
	admin := pq.QuoteIdentifier(adminUser)
	for _, stmt := range []string{
		"REASSIGN OWNED BY " + target + " TO " + admin,
		"DROP OWNED BY " + target,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("db %q: %s: %w", dbname, stmt, err)
		}
	}
	return nil
}

// updatePatroniPgHba adds/replaces hostssl+hostnossl rules for username in the
// Patroni DCS pg_hba list. If DCS has no pg_hba yet, the current rules are first
// seeded from pg_hba_file_rules so existing catch-all rules are preserved.
func (s *Service) updatePatroniPgHba(ctx context.Context, jobID, username string, ssl bool) error {
	cidr := s.proxyAllowedIP + "/32"
	var newRules []string
	if ssl {
		newRules = []string{
			"hostssl all " + username + " " + cidr + " scram-sha-256",
			"hostnossl all " + username + " " + cidr + " reject",
		}
	} else {
		newRules = []string{
			"host all " + username + " " + cidr + " scram-sha-256",
		}
	}
	return s.patchPatroniPgHba(ctx, jobID, username, newRules)
}

// removeFromPatroniPgHba removes all pg_hba entries for username, but only if DCS
// already owns pg_hba (so we don't accidentally take ownership on delete).
func (s *Service) removeFromPatroniPgHba(ctx context.Context, jobID, username string) error {
	return s.patchPatroniPgHba(ctx, jobID, username, nil)
}

// patchPatroniPgHba is the low-level helper: it reads the current DCS pg_hba,
// strips rules for username, prepends newRules, and PATCHes /config.
//
// If DCS has no pg_hba set yet (first call), it seeds the baseline from the
// PostgreSQL pg_hba_file_rules view so existing catch-all rules are not lost.
// If newRules is nil and DCS has no pg_hba, this is a no-op.
func (s *Service) patchPatroniPgHba(ctx context.Context, jobID, username string, newRules []string) error {
	job, err := s.store.Load(jobID)
	if err != nil {
		return fmt.Errorf("load job: %w", err)
	}
	secret, err := s.store.LoadSecret(jobID)
	if err != nil {
		return fmt.Errorf("load secret: %w", err)
	}
	candidates := append([]string{job.Request.PrimaryIP}, job.Request.StandbyIPs...)
	primary, err := s.findPrimary(ctx, candidates)
	if err != nil {
		return fmt.Errorf("discover primary: %w", err)
	}

	patroniURL := fmt.Sprintf("http://%s:8008/config", primary)

	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, patroniURL, nil)
	if err != nil {
		return fmt.Errorf("build patroni GET: %w", err)
	}
	getReq.SetBasicAuth(job.Request.AdminUsername, secret.AdminPassword)
	resp, err := s.httpClient.Do(getReq)
	if err != nil {
		return fmt.Errorf("GET patroni config: %w", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET patroni config: status %d", resp.StatusCode)
	}

	var cfg map[string]interface{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse patroni config: %w", err)
	}

	// Check whether DCS already owns pg_hba.
	var existing []string
	hasDCSPgHba := false
	if pg, ok := cfg["postgresql"].(map[string]interface{}); ok {
		if hba, ok := pg["pg_hba"].([]interface{}); ok {
			hasDCSPgHba = true
			for _, r := range hba {
				if line, ok := r.(string); ok {
					existing = append(existing, line)
				}
			}
		}
	}

	// If DCS has no pg_hba and we have nothing to add (remove call), skip entirely —
	// there are no DCS-managed rules to clean up.
	if !hasDCSPgHba && len(newRules) == 0 {
		return nil
	}

	// First time we touch DCS pg_hba: seed from the real pg_hba_file_rules so
	// existing catch-all rules are carried forward.
	if !hasDCSPgHba {
		p := job.Request.PostgresPort
		if p == 0 {
			p = 5432
		}
		existing, err = s.readPgHbaRules(ctx, primary, p, secret.PostgresUser, secret.PostgresPassword)
		if err != nil {
			return fmt.Errorf("seed pg_hba from file: %w", err)
		}
	}

	// Strip old rules for this user, prepend the new ones.
	filtered := make([]string, 0, len(existing))
	for _, rule := range existing {
		if f := strings.Fields(rule); len(f) >= 3 && f[2] == username {
			continue
		}
		filtered = append(filtered, rule)
	}
	allRules := append(newRules, filtered...)

	patch := map[string]interface{}{
		"postgresql": map[string]interface{}{
			"pg_hba": allRules,
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	patchReq, err := http.NewRequestWithContext(ctx, http.MethodPatch, patroniURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build patroni PATCH: %w", err)
	}
	patchReq.Header.Set("Content-Type", "application/json")
	patchReq.SetBasicAuth(job.Request.AdminUsername, secret.AdminPassword)
	patchResp, err := s.httpClient.Do(patchReq)
	if err != nil {
		return fmt.Errorf("PATCH patroni config: %w", err)
	}
	defer patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(patchResp.Body)
		return fmt.Errorf("PATCH patroni config: status %d: %s", patchResp.StatusCode, b)
	}
	return nil
}

// readPgHbaRules reads the current pg_hba.conf rules via pg_hba_file_rules and
// reconstructs them as strings suitable for Patroni's pg_hba DCS list.
func (s *Service) readPgHbaRules(ctx context.Context, host string, port int, user, password string) ([]string, error) {
	db, err := s.connect(ctx, host, port, "postgres", user, password)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT type,
		       array_to_string(database, ',') AS database,
		       array_to_string(user_name, ',')  AS username,
		       COALESCE(address, '')             AS address,
		       COALESCE(netmask, '')             AS netmask,
		       auth_method
		FROM pg_hba_file_rules
		WHERE error IS NULL
		ORDER BY line_number`)
	if err != nil {
		return nil, fmt.Errorf("query pg_hba_file_rules: %w", err)
	}
	defer rows.Close()

	var rules []string
	for rows.Next() {
		var typ, db2, usr, addr, netmask, method string
		if err := rows.Scan(&typ, &db2, &usr, &addr, &netmask, &method); err != nil {
			return nil, fmt.Errorf("scan pg_hba_file_rules: %w", err)
		}
		var rule string
		switch typ {
		case "local":
			rule = strings.Join([]string{typ, db2, usr, method}, " ")
		default:
			if netmask != "" {
				rule = strings.Join([]string{typ, db2, usr, addr, netmask, method}, " ")
			} else {
				rule = strings.Join([]string{typ, db2, usr, addr, method}, " ")
			}
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

/**
 * without.
 *
 * Params:
 *   slice []string - the slice ([]string)
 *   target string - the target string
 *
 * Returns:
 *   []string - the resulting []string
 */
func without(slice []string, target string) []string {
	out := make([]string, 0, len(slice))
	for _, v := range slice {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}
