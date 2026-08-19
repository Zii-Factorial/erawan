package pgsql

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	pgsqlcluster "erawan-cluster/internal/cluster/pgsql"
	pgsqldbmanager "erawan-cluster/internal/cluster/pgsql/dbmanager"
	"erawan-cluster/internal/render"

	"github.com/go-chi/chi/v5"
)

// Handler holds PostgreSQL cluster and DB manager services for HTTP route handling.
type Handler struct {
	cluster   *pgsqlcluster.Service
	dbmanager *pgsqldbmanager.Service
	proxyHost string
}

/**
 * New creates a Handler with the given services.
 *
 * Params:
 *   cluster *pgsqlcluster.Service - the cluster (*pgsqlcluster.Service)
 *   db *pgsqldbmanager.Service - the db (*pgsqldbmanager.Service)
 *   proxyHost string - the proxyHost string
 *
 * Returns:
 *   *Handler - the resulting *Handler
 */
func New(cluster *pgsqlcluster.Service, db *pgsqldbmanager.Service, proxyHost string) *Handler {
	return &Handler{cluster: cluster, dbmanager: db, proxyHost: proxyHost}
}

/**
 * Deploy.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) Deploy(w http.ResponseWriter, r *http.Request) {
	var req pgsqlcluster.DeployRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	job, err := h.cluster.Deploy(r.Context(), req)
	if err != nil {
		if job != nil {
			render.JSON(w, http.StatusUnprocessableEntity, render.Envelope{
				"status":  "error",
				"message": err.Error(),
				"data":    job,
			})
			return
		}
		render.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	secret, _ := h.cluster.GetSecret(job.ID)
	render.Accepted(w, "PostgreSQL cluster deployment started", struct {
		*pgsqlcluster.Job
		Secret *pgsqlcluster.StoredSecret `json:"secret,omitempty"`
	}{job, secret})
}

/**
 * GetJob.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) GetJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")
	job, err := h.cluster.Get(jobID)
	if err != nil {
		render.Error(w, http.StatusNotFound, err.Error())
		return
	}
	secret, _ := h.cluster.GetSecret(jobID)
	job.Request.SSHPrivateKeyPath = ""
	render.OK(w, "success", struct {
		*pgsqlcluster.Job
		Secret *pgsqlcluster.StoredSecret `json:"secret,omitempty"`
	}{job, secret})
}

/**
 * ListJobs.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) ListJobs(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	jobs, err := h.cluster.List(limit)
	if err != nil {
		render.Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	render.OK(w, "success", jobs)
}

/**
 * ResumeJob.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) ResumeJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")
	var req pgsqlcluster.ResumeRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	job, err := h.cluster.Resume(r.Context(), jobID, req)
	if err != nil {
		if job != nil {
			render.JSON(w, http.StatusUnprocessableEntity, render.Envelope{
				"status":  "error",
				"message": err.Error(),
				"data":    job,
			})
			return
		}
		render.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	secret, _ := h.cluster.GetSecret(job.ID)
	render.Accepted(w, "PostgreSQL cluster job resumed", struct {
		*pgsqlcluster.Job
		Secret *pgsqlcluster.StoredSecret `json:"secret,omitempty"`
	}{job, secret})
}

/**
 * RecoverJob triggers a post-outage cluster recovery for the deploy job identified
 * by {jobID}. It runs cluster_bootstrap and verify_cluster using stored credentials
 * — no request body is required.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) RecoverJob(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "jobID")
	job, err := h.cluster.Recover(r.Context(), jobID)
	if err != nil {
		render.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	render.Accepted(w, "PostgreSQL cluster recovery started", job)
}

// releaseDCSRequest is the body of the control-plane DCS release. It accepts a
// single job_id, or job_ids for releasing several clusters in one call —
// useful for clearing tenants left behind by deletes that predate the proxy
// delete releasing them.
//
// Both forms name their clusters explicitly. There is deliberately no "release
// everything unused" mode: nothing in this system records that a cluster was
// deleted, so a stopped cluster and a decommissioned one are indistinguishable,
// and purging a stopped cluster's keys would strip the Patroni state it expects
// to find on its next start.
type releaseDCSRequest struct {
	JobID  string   `json:"job_id"`
	JobIDs []string `json:"job_ids"`
	// Confirm is required for the batch form only, so a single release keeps
	// working exactly as before.
	Confirm bool `json:"confirm"`
}

/**
 * jobIDs normalizes the two accepted body shapes into one de-duplicated list.
 *
 * Receiver:
 *   req releaseDCSRequest - value receiver; the method operates on a copy
 *
 * Returns:
 *   []string - the job IDs to release, in the order given
 *   error - non-nil when the body names no job, or a batch is unconfirmed
 */
func (req releaseDCSRequest) jobIDs() ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(req.JobIDs)+1)
	for _, id := range append([]string{req.JobID}, req.JobIDs...) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, errors.New("job_id or job_ids is required")
	}
	if len(out) > 1 && !req.Confirm {
		return nil, errors.New(`releasing several clusters at once requires "confirm": true`)
	}
	return out, nil
}

// releaseDCSResult is one cluster's outcome in a batch release.
type releaseDCSResult struct {
	JobID        string `json:"job_id"`
	Status       string `json:"status"`
	ReleaseJobID string `json:"release_job_id,omitempty"`
	Error        string `json:"error,omitempty"`
}

// serviceOpRequest is the body of the stop/start cluster endpoints: the deploy
// job that owns the cluster, passed as payload like the member endpoints.
type serviceOpRequest struct {
	JobID string `json:"job_id"`
}

func decodeServiceOpRequest(r *http.Request) (string, error) {
	var req serviceOpRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		return "", err
	}
	jobID := strings.TrimSpace(req.JobID)
	if jobID == "" {
		return "", errors.New("job_id is required")
	}
	return jobID, nil
}

/**
 * StartJob starts a stopped cluster. Starting is the same operation as
 * post-outage recovery — cluster_bootstrap re-registers the cluster in the
 * DCS and starts Patroni on all nodes without touching data directories —
 * so this delegates to Recover. The deploy job ID comes in the request body.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) StartJob(w http.ResponseWriter, r *http.Request) {
	jobID, err := decodeServiceOpRequest(r)
	if err != nil {
		render.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := h.cluster.Recover(r.Context(), jobID)
	if err != nil {
		render.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	render.Accepted(w, "PostgreSQL cluster start initiated", job)
}

/**
 * StopJob gracefully stops the cluster owned by the job_id in the request
 * body without touching any data: Patroni is stopped on standbys first, then
 * the primary, then etcd on all nodes. Restart with StartJob.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) StopJob(w http.ResponseWriter, r *http.Request) {
	jobID, err := decodeServiceOpRequest(r)
	if err != nil {
		render.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := h.cluster.Stop(r.Context(), jobID)
	if err != nil {
		render.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	render.Accepted(w, "PostgreSQL cluster stop initiated", job)
}

/**
 * ReleaseDCS releases the cluster's namespace on the shared control-plane etcd:
 * its Patroni keys, its etcd user and role, and its nodes' access to the
 * control plane. Call it when a cluster is decommissioned — the tenant's
 * objects are named after the cluster, so leaving them behind accumulates state
 * on shared infrastructure and would be inherited by a later cluster of the
 * same name.
 *
 * It removes coordination state only, never PostgreSQL data — but a cluster
 * whose nodes are still running loses its DCS, so release it after stopping or
 * destroying the nodes.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) ReleaseDCS(w http.ResponseWriter, r *http.Request) {
	var req releaseDCSRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	jobIDs, err := req.jobIDs()
	if err != nil {
		render.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	// One job keeps the original response verbatim, so existing callers are
	// unaffected by the batch form.
	if len(jobIDs) == 1 {
		job, err := h.cluster.ReleaseDCS(r.Context(), jobIDs[0])
		if err != nil {
			render.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		render.Accepted(w, "PostgreSQL control-plane DCS release initiated", job)
		return
	}

	// A batch never fails as a whole: releasing four tenants and failing on the
	// fifth must not look like nothing happened, or the caller retries the four
	// that already succeeded and cannot tell which one actually needs attention.
	results := make([]releaseDCSResult, 0, len(jobIDs))
	var released int
	for _, jobID := range jobIDs {
		job, err := h.cluster.ReleaseDCS(r.Context(), jobID)
		switch {
		case err != nil:
			results = append(results, releaseDCSResult{JobID: jobID, Status: "failed", Error: err.Error()})
		default:
			released++
			res := releaseDCSResult{JobID: jobID, Status: "releasing"}
			if job != nil {
				res.ReleaseJobID = job.ID
			}
			results = append(results, res)
		}
	}
	render.Accepted(w, fmt.Sprintf("PostgreSQL control-plane DCS release initiated for %d of %d clusters", released, len(jobIDs)),
		map[string]any{"released": released, "requested": len(jobIDs), "results": results})
}

/**
 * AddMember.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) AddMember(w http.ResponseWriter, r *http.Request) {
	var req pgsqlcluster.AddMemberRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	job, err := h.cluster.AddMember(r.Context(), req)
	if err != nil {
		render.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	render.Accepted(w, "PostgreSQL cluster member addition started", job)
}

/**
 * RemoveMember.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	var req pgsqlcluster.RemoveMemberRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	job, err := h.cluster.RemoveMember(r.Context(), req)
	if err != nil {
		render.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	render.Accepted(w, "PostgreSQL cluster member removal started", job)
}

/**
 * Metrics.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) Metrics(w http.ResponseWriter, r *http.Request) {
	var req pgsqlcluster.MetricRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.JobID != "" {
		_, _, _, _, nodeIPs, err := h.cluster.ConnectionInfo(r.Context(), req.JobID)
		if err != nil {
			render.Error(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		req.NodeIPs = nodeIPs
	}

	req.Host = h.proxyHost
	req.Port = req.ProxyPort

	if err := pgsqlcluster.ValidateMetricRequest(&req); err != nil {
		render.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	result := h.cluster.CollectMetrics(r.Context(), req)
	render.OK(w, "metrics collected", result)
}

/**
 * CreateUser.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req pgsqldbmanager.CreateUserRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := h.dbmanager.CreateUser(r.Context(), req); err != nil {
		render.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	render.OK(w, "user created", nil)
}

/**
 * UpdateUser.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	var req pgsqldbmanager.UpdateUserRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := h.dbmanager.UpdateUser(r.Context(), req); err != nil {
		render.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	render.OK(w, "user renamed", nil)
}

/**
 * ResetPassword.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var req pgsqldbmanager.ResetPasswordRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := h.dbmanager.ResetPassword(r.Context(), req); err != nil {
		render.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	render.OK(w, "password reset", nil)
}

/**
 * DeleteUser.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	var req pgsqldbmanager.DeleteUserRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := h.dbmanager.DeleteUser(r.Context(), req); err != nil {
		render.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	render.OK(w, "user deleted", nil)
}

/**
 * GetConnectionLimit reports the configured connection limit of the cluster
 * owned by the job_id query parameter, plus the live max_connections value and
 * pending-restart flag on every node.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) GetConnectionLimit(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimSpace(r.URL.Query().Get("job_id"))
	if jobID == "" {
		render.Error(w, http.StatusBadRequest, "job_id query parameter is required")
		return
	}
	status, err := h.dbmanager.GetConnectionLimit(r.Context(), jobID)
	if err != nil {
		render.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	render.OK(w, "connection limit", status)
}

/**
 * SetConnectionLimit changes max_connections cluster-wide via the Patroni DCS
 * for the cluster owned by the job_id in the request body, persists the new
 * value into the job spec, and rolls the required PostgreSQL restarts. Partial
 * failures return 422 with the per-node results as data, mirroring the Deploy
 * error contract.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) SetConnectionLimit(w http.ResponseWriter, r *http.Request) {
	var req pgsqldbmanager.SetConnectionLimitRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	status, err := h.dbmanager.SetConnectionLimit(r.Context(), req)
	if err != nil {
		if status != nil {
			render.JSON(w, http.StatusUnprocessableEntity, render.Envelope{
				"status":  "error",
				"message": err.Error(),
				"data":    status,
			})
			return
		}
		render.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	render.OK(w, "connection limit updated", status)
}

/**
 * CreateDatabase.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) CreateDatabase(w http.ResponseWriter, r *http.Request) {
	var req pgsqldbmanager.CreateDatabaseRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := h.dbmanager.CreateDatabase(r.Context(), req); err != nil {
		render.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	render.OK(w, "database created", nil)
}

/**
 * UpdateDatabase.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) UpdateDatabase(w http.ResponseWriter, r *http.Request) {
	var req pgsqldbmanager.UpdateDatabaseRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := h.dbmanager.UpdateDatabase(r.Context(), req); err != nil {
		render.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	render.OK(w, "database renamed", nil)
}

/**
 * DeleteDatabase.
 *
 * Receiver:
 *   h *Handler - pointer receiver; the method may mutate this Handler instance
 *
 * Params:
 *   w http.ResponseWriter - the HTTP response writer the result is written to
 *   r *http.Request - the incoming HTTP request
 */
func (h *Handler) DeleteDatabase(w http.ResponseWriter, r *http.Request) {
	var req pgsqldbmanager.DeleteDatabaseRequest
	if err := render.DecodeJSON(r, &req); err != nil {
		render.Error(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := h.dbmanager.DeleteDatabase(r.Context(), req); err != nil {
		render.Error(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	render.OK(w, "database deleted", nil)
}
