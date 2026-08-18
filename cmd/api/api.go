package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof handlers on the loopback pprof server
	"path/filepath"
	"time"

	mysqlcluster "erawan-cluster/internal/cluster/mysql"
	mysqldbmanager "erawan-cluster/internal/cluster/mysql/dbmanager"
	pgsqlcluster "erawan-cluster/internal/cluster/pgsql"
	"erawan-cluster/internal/cluster/pgsql/dbmanager"
	"erawan-cluster/internal/haproxy"
	"erawan-cluster/internal/security"

	haproxyapi "erawan-cluster/cmd/api/haproxy"
	mysqlapi "erawan-cluster/cmd/api/mysql"
	pgsqlapi "erawan-cluster/cmd/api/pgsql"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// application is the long-lived dependency container shared by every HTTP
// handler. It holds the request-time config plus one service per subsystem:
// the HAProxy renderer, the two SQL cluster engines and their database
// managers, and the optional payload cipher. It is built once, in
// buildApplication (setup.go), and never mutated after start-up.
type application struct {
	config               config
	haproxy              *haproxy.Service
	mysqlCluster         *mysqlcluster.Service
	pgsqlCluster         *pgsqlcluster.Service
	pgsqlDB              *dbmanager.Service
	mysqlDB              *mysqldbmanager.Service
	cipher               *security.Cipher
	baseDir              string
	enablePprof          bool
	shutdownDrainSeconds int
	jobDB                *sql.DB
}

/**
 * mount builds the chi router: it installs the cross-cutting middleware chain
 * (request IDs, logging, recovery, timeout, API-key auth, and the optional
 * encrypt/decrypt + body-limit pipeline) and then registers the route groups
 * for docs, HAProxy, and each cluster engine. A new engine adds one Route block.
 *
 * Receiver:
 *   app *application - supplies the per-subsystem services and config the
 *     handlers and middleware are bound to.
 * Returns:
 *   *chi.Mux - the fully assembled router, ready to serve.
 */
func (app *application) mount() *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Minute))
	r.Use(security.APIKeyMiddleware(app.config.apiKey))
	r.Use(security.EncryptMiddleware(app.cipher))
	r.Use(bodyLimit(2 << 20)) // 2 MB covers ~1.5 MB plaintext after AES-GCM base64 overhead
	r.Use(security.DecryptMiddleware(app.cipher))

	r.Get("/health", app.healthCheckHandler)
	r.Get("/docs", app.docsHandler)

	haproxyH := haproxyapi.New(app.haproxy)
	r.Route("/haproxy", func(r chi.Router) {
		r.Post("/config/mysql", haproxyH.CreateMySQLConfig)
		r.Patch("/config/mysql", haproxyH.AddMySQLMember)
		r.Post("/config/pgsql", haproxyH.CreatePGSQLConfig)
		r.Patch("/config/pgsql", haproxyH.AddPGSQLMember)
		r.Delete("/config", haproxyH.DeleteConfig)
		r.Get("/configs", haproxyH.ListConfigs)
		r.Get("/configs/download", haproxyH.DownloadZip)
		r.Post("/reload", haproxyH.Reload)
	})

	mysqlH := mysqlapi.New(app.mysqlCluster, app.mysqlDB, app.config.proxyHost)
	r.Route("/cluster/mysql", func(r chi.Router) {
		r.Post("/deploy", mysqlH.Deploy)
		r.Post("/metrics", mysqlH.Metrics)
		r.Get("/jobs", mysqlH.ListJobs)
		r.Get("/jobs/{jobID}", mysqlH.GetJob)
		r.Post("/jobs/{jobID}/resume", mysqlH.ResumeJob)
		r.Post("/jobs/{jobID}/recover", mysqlH.RecoverJob)
		r.Post("/jobs/{jobID}/rollback", mysqlH.RollbackJob)
		r.Post("/start", mysqlH.StartJob)
		r.Post("/stop", mysqlH.StopJob)
		r.Post("/members", mysqlH.AddMember)
		r.Delete("/members", mysqlH.RemoveMember)
		r.Get("/connection-limit", mysqlH.GetConnectionLimit)
		r.Put("/connection-limit", mysqlH.SetConnectionLimit)
		r.Post("/users", mysqlH.CreateUser)
		r.Patch("/users", mysqlH.UpdateUser)
		r.Put("/users/password", mysqlH.ResetPassword)
		r.Delete("/users", mysqlH.DeleteUser)
		r.Post("/databases", mysqlH.CreateDatabase)
		r.Patch("/databases", mysqlH.UpdateDatabase)
		r.Delete("/databases", mysqlH.DeleteDatabase)
	})

	pgsqlH := pgsqlapi.New(app.pgsqlCluster, app.pgsqlDB, app.config.proxyHost)
	r.Route("/cluster/pgsql", func(r chi.Router) {
		r.Post("/deploy", pgsqlH.Deploy)
		r.Post("/metrics", pgsqlH.Metrics)
		r.Get("/jobs", pgsqlH.ListJobs)
		r.Get("/jobs/{jobID}", pgsqlH.GetJob)
		r.Post("/jobs/{jobID}/resume", pgsqlH.ResumeJob)
		r.Post("/jobs/{jobID}/recover", pgsqlH.RecoverJob)
		r.Post("/start", pgsqlH.StartJob)
		r.Post("/stop", pgsqlH.StopJob)
		r.Post("/members", pgsqlH.AddMember)
		r.Delete("/members", pgsqlH.RemoveMember)
		// Releases the cluster's tenant namespace on the shared control-plane
		// etcd. Only meaningful for clusters deployed with SHARED_CONTROL_PLANE.
		r.Delete("/dcs", pgsqlH.ReleaseDCS)
		r.Get("/connection-limit", pgsqlH.GetConnectionLimit)
		r.Put("/connection-limit", pgsqlH.SetConnectionLimit)
		r.Post("/users", pgsqlH.CreateUser)
		r.Patch("/users", pgsqlH.UpdateUser)
		r.Put("/users/password", pgsqlH.ResetPassword)
		r.Delete("/users", pgsqlH.DeleteUser)
		r.Post("/databases", pgsqlH.CreateDatabase)
		r.Patch("/databases", pgsqlH.UpdateDatabase)
		r.Delete("/databases", pgsqlH.DeleteDatabase)
	})

	return r
}

/**
 * run starts the HTTP server and blocks until it stops. A background goroutine
 * watches ctx; when it is cancelled (on SIGINT/SIGTERM) the server is given a
 * 30-second window to drain in-flight requests and background jobs before
 * exiting. The long write timeout accommodates streaming Ansible deploy output.
 *
 * Receiver:
 *   app *application - provides the listen address, the cluster services to
 *     drain on shutdown, and the pprof toggle.
 * Params:
 *   ctx context.Context - cancelled on OS shutdown signals; triggers graceful
 *     shutdown when done.
 *   mux *chi.Mux - the router handling all requests.
 * Returns:
 *   error - any listen/serve error except http.ErrServerClosed (clean stop),
 *     which is reported as success (nil).
 */
func (app *application) run(ctx context.Context, mux *chi.Mux) error {
	defer func() {
		if app.jobDB != nil {
			_ = app.jobDB.Close()
		}
	}()

	srv := &http.Server{
		Addr:         app.config.addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  time.Minute,
	}

	if app.enablePprof {
		app.startPprof()
	}

	go func() {
		<-ctx.Done()

		// Phase 1: stop accepting new HTTP requests and drain in-flight ones.
		// Handlers return 202 immediately for long-running jobs so this is quick.
		httpCtx, httpCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer httpCancel()
		_ = srv.Shutdown(httpCtx)

		// Phase 2: wait for background Ansible jobs to write their final status
		// to the store before the process exits. Root ctx cancellation already
		// sent SIGKILL to in-flight Ansible processes; we are just waiting for
		// the goroutines to finish their status-update DB writes.
		drainTimeout := time.Duration(app.shutdownDrainSeconds) * time.Second
		drainCtx, drainCancel := context.WithTimeout(context.Background(), drainTimeout)
		defer drainCancel()
		app.mysqlCluster.Wait(drainCtx)
		app.pgsqlCluster.Wait(drainCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

/**
 * startPprof serves the net/http/pprof endpoints on the loopback interface only,
 * so profiling data is never exposed on the public listener. Enabled via
 * ENABLE_PPROF; intended for diagnosing leaks/CPU in a controlled environment.
 * It launches the pprof server in a background goroutine and returns immediately.
 *
 * Receiver:
 *   app *application - receiver only; no fields are read.
 */
func (app *application) startPprof() {
	go func() {
		log.Printf("pprof listening on 127.0.0.1:6060 (loopback only)")
		pprofSrv := &http.Server{
			Addr:              "127.0.0.1:6060",
			ReadHeaderTimeout: 5 * time.Second,
		}
		if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("pprof server: %v", err)
		}
	}()
}

/**
 * bodyLimit returns middleware that caps the request body at limit bytes,
 * rejecting larger payloads. It guards against memory exhaustion from oversized
 * (or maliciously large) requests before the body is read or decrypted.
 *
 * Params:
 *   limit int64 - maximum allowed request body size, in bytes.
 * Returns:
 *   func(http.Handler) http.Handler - middleware wrapping each handler with a
 *     http.MaxBytesReader-bounded body.
 */
func bodyLimit(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

/**
 * docsHandler serves the static API documentation page (index.html) from the
 * project base directory.
 *
 * Receiver:
 *   app *application - supplies baseDir, the root the file is resolved against.
 * Params:
 *   w http.ResponseWriter - the response writer the file is streamed to.
 *   r *http.Request - the incoming request, forwarded to http.ServeFile.
 */
func (app *application) docsHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(app.baseDir, "index.html"))
}
