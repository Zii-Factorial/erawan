package pgsql

import (
	"context"
	"fmt"
	"time"

	"erawan-cluster/internal/cluster/core"
)

type Service struct {
	// ctx is the long-lived base context for background jobs, which intentionally
	// outlive the originating HTTP request. It is set once at start-up via
	// SetContext to the process signal context, so a shutdown cancels in-flight
	// Ansible runs. It is never derived from a per-request context.
	ctx              context.Context
	store            Store
	runner           *Runner
	collector        *Collector
	steps            []step
	sshUser          string
	sshKeyPath       string
	launcher         *core.Launcher
	start            func(func())
	runDeployStep    func(context.Context, runConfig) StepResult
	runAddMemberStep func(context.Context, memberRunConfig) StepResult
	runRemMemberStep func(context.Context, memberRunConfig) StepResult
	runStopStep      func(context.Context, runConfig) StepResult
	runDCSProvision  func(context.Context, dcsRunConfig) StepResult
	runDCSCleanup    func(context.Context, dcsRunConfig) StepResult
}

type step = core.Step

// controlPlaneDCSStep provisions this cluster's tenant namespace on the shared
// control-plane etcd and installs the control plane's CA on its nodes. Unlike
// every other deploy step it does not run the deploy playbook: it runs the
// shared, engine-agnostic playbook against a different inventory (the control
// plane plus this cluster's nodes), so runDeploy dispatches it separately.
// Skipped entirely for clusters that keep their DCS on their own nodes.
const controlPlaneDCSStep = "control_plane_dcs"

// dcsExecTimeout bounds a control-plane DCS run. Its work is a handful of
// etcdctl calls plus one small file copy per node, so it never needs the full
// step budget — but it does include a bounded wait for nodes that may have been
// created seconds ago (add-member), hence minutes rather than seconds.
const dcsExecTimeout = 10 * time.Minute

// scaleRebootWait bounds how long a recovery step waits for a node that went
// down mid-run to accept connections again. A vertical scale is a stop, an
// offering change and a start, which is minutes rather than seconds — but a
// node that is not back within this is down for a reason a retry will not fix,
// and the job should say so instead of hanging.
const scaleRebootWait = 5 * time.Minute

// defaultMaxConcurrentJobs bounds concurrent background jobs until configured.
const defaultMaxConcurrentJobs = 4

// clusterBootstrapMinExecTimeout floors the process-level exec timeout for the
// cluster_bootstrap step. Its internal waits (etcd health, Patroni leader
// election, Postgres port open, replica bootstrap) are fixed constants in
// cluster_bootstrap/tasks/main.yml, independent of StepTimeoutSeconds, and
// sum to ~1020s worst case -- comfortably exceeding the previous default 900s
// StepTimeoutSeconds process timeout on a cold/slow boot (e.g. right after a
// vertical CPU/RAM resize, which reboots the node and can stretch every one
// of those waits at once).
const clusterBootstrapMinExecTimeout = 25 * time.Minute

// verifyClusterExecBuffer covers verify_cluster's own fixed-cost tasks
// (systemctl checks, local /patroni status) on top of the step_timeout_seconds
// budget its three retry-loops now share (see verify_cluster/tasks/main.yml).
const verifyClusterExecBuffer = 2 * time.Minute

// execTimeoutForTag returns the process-level exec timeout for a single
// deploy/recovery step. Most steps run their internal waits within the base
// StepTimeoutSeconds-derived budget, but cluster_bootstrap and verify_cluster
// need adjustment: see the constants above.
func execTimeoutForTag(tag string, base time.Duration) time.Duration {
	switch tag {
	case "cluster_bootstrap":
		if base < clusterBootstrapMinExecTimeout {
			return clusterBootstrapMinExecTimeout
		}
		return base
	case "verify_cluster":
		return base + verifyClusterExecBuffer
	case controlPlaneDCSStep:
		return dcsExecTimeout
	default:
		return base
	}
}

// addMemberFixedOverhead covers add_member.yml's plays that run ahead of the
// replica clone/verify: preflight's SSH wait, add_member_register's etcd
// housekeeping, and add_member_join's own etcd/Patroni startup waits. All are
// fixed costs independent of StepTimeoutSeconds.
const addMemberFixedOverhead = 15 * time.Minute

// removeMemberExecBuffer covers remove_member.yml's stop/etcd-removal plays,
// which have no meaningful retry loops of their own -- verify_cluster (see
// verifyClusterExecBuffer) is the only real budget consumer.
const removeMemberExecBuffer = 2 * time.Minute

// memberExecTimeout returns the process-level exec timeout for the
// add_member.yml / remove_member.yml runs. Each bundles several Ansible plays
// -- including a step_timeout_seconds-bounded replica wait (add_member only)
// and the now-shared verify_cluster budget -- into a single ansible-playbook
// process governed by one Go-side timeout. That timeout must cover the sum
// of what it bundles, not just one wait's nominal ceiling, or the process
// gets killed (context deadline) mid-run before the later plays -- especially
// add_member_join's replica clone, which can legitimately run close to the
// full step_timeout_seconds on a large database -- ever get a chance to
// finish or report a real error.
func memberExecTimeout(stepName string, base time.Duration) time.Duration {
	switch stepName {
	case "add_member":
		// base covers add_member_join's replica wait once, plus the
		// verify_cluster budget it shares the process with once more.
		return 2*base + addMemberFixedOverhead
	case "remove_member":
		return base + removeMemberExecBuffer
	default:
		return base
	}
}

/**
 * NewService.
 *
 * Params:
 *   store *Store - the store (*Store)
 *   runner *Runner - the runner (*Runner)
 *
 * Returns:
 *   *Service - the resulting *Service
 */
func NewService(store Store, runner *Runner) *Service {
	svc := &Service{
		ctx:       context.Background(),
		store:     store,
		runner:    runner,
		collector: NewCollector(),
		steps: []step{
			{Name: "preflight", Tag: "preflight"},
			// Runs before base_config: the nodes must already trust the control
			// plane and its tenant user must exist before any Patroni config
			// referencing them is written.
			{Name: controlPlaneDCSStep, Tag: controlPlaneDCSStep},
			{Name: "base_config", Tag: "base_config"},
			{Name: "primary_config", Tag: "primary_config"},
			{Name: "standby_config", Tag: "standby_config"},
			{Name: "cluster_bootstrap", Tag: "cluster_bootstrap"},
			{Name: "verify_cluster", Tag: "verify_cluster"},
			{Name: "setup_exporters", Tag: "setup_exporters"},
			{Name: "init_app_db", Tag: "init_app_db", Skippable: true},
		},
	}
	svc.launcher = core.NewLauncher(defaultMaxConcurrentJobs)
	svc.start = svc.launcher.Go
	if runner != nil {
		svc.runDeployStep = runner.RunDeployStep
		svc.runAddMemberStep = runner.RunAddMember
		svc.runRemMemberStep = runner.RunRemoveMember
		svc.runStopStep = runner.RunStop
		svc.runDCSProvision = runner.RunDCSProvision
		svc.runDCSCleanup = runner.RunDCSCleanup
	}
	return svc
}

/**
 * SetContext.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 */
func (s *Service) SetContext(ctx context.Context) {
	s.ctx = ctx
}

/**
 * SetMaxConcurrentJobs bounds how many background jobs run at once. It must be
 * called at start-up, before any job is launched.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   n int - the n value
 */
func (s *Service) SetMaxConcurrentJobs(n int) {
	s.launcher = core.NewLauncher(n)
	s.start = s.launcher.Go
}

/**
 * Wait blocks until all in-flight background jobs finish or ctx is done. Used
 * during graceful shutdown to drain running jobs.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 */
func (s *Service) Wait(ctx context.Context) {
	s.launcher.Wait(ctx)
}

/**
 * SetSSHConfig.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   user string - the user string
 *   privateKeyPath string - the privateKeyPath string
 *
 * Returns:
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) SetSSHConfig(user, privateKeyPath string) error {
	normalizedUser, normalizedKeyPath, err := ValidateServiceSSHConfig(user, privateKeyPath)
	if err != nil {
		return err
	}
	s.sshUser = normalizedUser
	s.sshKeyPath = normalizedKeyPath
	return nil
}

/**
 * Deploy.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   req DeployRequest - the req (DeployRequest)
 *
 * Returns:
 *   *Job - the resulting *Job
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) Deploy(ctx context.Context, req DeployRequest) (*Job, error) {
	_ = ctx
	if err := ValidateDeployRequest(&req); err != nil {
		return nil, err
	}
	if err := s.hydrateStoredSSHConfig(nil); err != nil {
		return nil, err
	}

	// Decided once, here, and recorded on the job: every later operation on
	// this cluster reads the spec, never the environment, so flipping
	// SHARED_CONTROL_PLANE later cannot re-point a live cluster's DCS.
	controlPlaneDCS := s.runner != nil && s.runner.ControlPlaneEnabled()

	job := &Job{
		ID:                newJobID(),
		Status:            JobStatusRunning,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
		LastCompletedStep: -1,
		Request: StoredSpec{
			ClusterName:        req.ClusterName,
			PrimaryIP:          req.PrimaryIP,
			StandbyIPs:         req.StandbyIPs,
			AdminUsername:      req.AdminUsername,
			NewUser:            req.NewUser,
			NewUserSSLRequired: req.NewUserSSLRequiredEnabled(),
			NewUserSuperuser:   req.NewUserSuperuserEnabled(),
			NewDB:              req.NewDB,
			SSHUser:            s.sshUser,
			SSHPrivateKeyPath:  s.sshKeyPath,
			SSHPort:            req.SSHPort,
			PostgresPort:       req.PostgresPort,
			PostgresVersion:    req.PostgresVersion,
			ConnectionLimit:    req.ConnectionLimit,
			StepTimeoutSeconds: req.StepTimeoutSeconds,
			ControlPlaneDCS:    controlPlaneDCS,
		},
		Steps: make([]StepResult, 0, len(s.steps)),
	}

	s.updateJobProgress(job)
	if err := s.store.Save(job); err != nil {
		return nil, err
	}

	secrets := SecretInput{
		PostgresPassword:   stringOrGenerated(req.PostgresPassword),
		ReplicatorPassword: stringOrGenerated(req.ReplicatorPassword),
		AdminPassword:      stringOrGenerated(req.AdminPassword),
		NewUserPassword:    req.NewUserPassword,
		ExporterPassword:   stringOrGenerated(""),
	}
	// The tenant's etcd credential is generated once and never rotated by any
	// later operation: recover and add-member rewrite the control plane's copy
	// of it but do NOT rewrite patroni.yml on the existing nodes, so a rotation
	// would leave running nodes authenticating with a password the control
	// plane no longer accepts.
	dcsUser := ""
	if controlPlaneDCS {
		secrets.DCSPassword = stringOrGenerated("")
		dcsUser = s.runner.ControlPlaneTenantUser(req.ClusterName)
	}
	if err := s.store.SaveSecret(job.ID, StoredSecret{
		PostgresUser:       defaultPostgresSuperuser,
		PostgresPassword:   secrets.PostgresPassword,
		ReplicatorUser:     defaultReplicationUser,
		ReplicatorPassword: secrets.ReplicatorPassword,
		AdminPassword:      secrets.AdminPassword,
		ExporterPassword:   secrets.ExporterPassword,
		DCSUser:            dcsUser,
		DCSPassword:        secrets.DCSPassword,
	}); err != nil {
		return nil, err
	}

	bgJob, err := s.store.Load(job.ID)
	if err != nil {
		return nil, err
	}
	resetHostKeys := req.ResetHostKeys
	s.start(func() {
		_ = s.executeFrom(s.ctx, bgJob, 0, secrets, resetHostKeys)
	})
	return job, nil
}

/**
 * Resume.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   jobID string - the jobID string
 *   req ResumeRequest - the req (ResumeRequest)
 *
 * Returns:
 *   *Job - the resulting *Job
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) Resume(ctx context.Context, jobID string, req ResumeRequest) (*Job, error) {
	_ = ctx
	secret, err := ValidateResumeSecrets(req)
	if err != nil {
		return nil, err
	}

	// Atomically validate status and transition to Running so concurrent
	// Resume calls (e.g. during a brief VIP overlap) cannot both win.
	var job *Job
	var startIndex int
	if err := s.store.Update(jobID, func(j *Job) error {
		switch j.Status {
		case JobStatusCompleted:
			return fmt.Errorf("job %s already completed; use the recover endpoint to restart the cluster after an outage", jobID)
		case JobStatusRunning:
			return fmt.Errorf("job %s is already running", jobID)
		}
		if err := s.hydrateStoredSSHConfig(j); err != nil {
			return err
		}
		startIndex = j.LastCompletedStep + 1
		if startIndex >= len(s.steps) {
			j.Status = JobStatusCompleted
			j.Error = ""
			job = j
			return nil
		}
		if j.Request.NewUser != "" && secret.NewUserPassword == "" {
			return fmt.Errorf("new_user_password is required to resume job %s", jobID)
		}
		j.Status = JobStatusRunning
		j.Error = ""
		s.updateJobProgress(j)
		job = j
		return nil
	}); err != nil {
		return nil, err
	}

	if job.Status == JobStatusCompleted {
		return job, nil
	}

	// Loaded unconditionally now: besides filling in omitted passwords it also
	// carries the control-plane DCS credential, which the caller never supplies
	// and which must survive the SaveSecret below.
	storedSecret, loadErr := s.store.LoadSecret(job.ID)
	dcsUser := ""
	if loadErr == nil {
		if secret.PostgresPassword == "" {
			secret.PostgresPassword = storedSecret.PostgresPassword
		}
		if secret.ReplicatorPassword == "" {
			secret.ReplicatorPassword = storedSecret.ReplicatorPassword
		}
		if secret.AdminPassword == "" {
			secret.AdminPassword = storedSecret.AdminPassword
		}
		if secret.ExporterPassword == "" {
			secret.ExporterPassword = storedSecret.ExporterPassword
		}
		secret.DCSPassword = storedSecret.DCSPassword
		dcsUser = storedSecret.DCSUser
	}
	if job.Request.ControlPlaneDCS && secret.DCSPassword == "" {
		return nil, fmt.Errorf("job %s uses the shared control-plane DCS but its stored secret has no DCS password; deploy a new cluster instead of resuming", jobID)
	}
	if secret.PostgresPassword == "" {
		secret.PostgresPassword = stringOrGenerated("")
	}
	if secret.ReplicatorPassword == "" {
		secret.ReplicatorPassword = stringOrGenerated("")
	}
	if secret.AdminPassword == "" {
		secret.AdminPassword = stringOrGenerated("")
	}
	if secret.ExporterPassword == "" {
		secret.ExporterPassword = stringOrGenerated("")
	}
	if err := s.store.SaveSecret(job.ID, StoredSecret{
		PostgresUser:       defaultPostgresSuperuser,
		PostgresPassword:   secret.PostgresPassword,
		ReplicatorUser:     defaultReplicationUser,
		ReplicatorPassword: secret.ReplicatorPassword,
		AdminPassword:      secret.AdminPassword,
		ExporterPassword:   secret.ExporterPassword,
		DCSUser:            dcsUser,
		DCSPassword:        secret.DCSPassword,
	}); err != nil {
		return nil, err
	}

	bgJob, err := s.store.Load(job.ID)
	if err != nil {
		return nil, err
	}
	resetHostKeys := req.ResetHostKeys
	s.start(func() {
		_ = s.executeFrom(s.ctx, bgJob, startIndex, secret, resetHostKeys)
	})
	return job, nil
}

/**
 * Recover launches a new recovery job against the cluster owned by jobID, running
 * cluster_bootstrap and verify_cluster. Use after a complete datacenter outage:
 * this re-registers the cluster in the DCS (etcd/consul) and restarts Patroni on
 * all nodes without touching data directories. Stored secrets are used so no
 * passwords are required at call time.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   jobID string - ID of the original completed or failed deploy job
 *
 * Returns:
 *   *Job - the new running recovery job
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) Recover(ctx context.Context, jobID string) (*Job, error) {
	_ = ctx
	deployJob, err := s.store.Load(jobID)
	if err != nil {
		return nil, err
	}
	if err := s.hydrateStoredSSHConfig(deployJob); err != nil {
		return nil, err
	}
	if deployJob.Status == JobStatusRunning {
		return nil, fmt.Errorf("job %s is currently running; wait for it to finish before recovering", jobID)
	}
	if deployJob.Status == core.JobStatusRolledBack {
		return nil, fmt.Errorf("job %s was rolled back; run a new deploy instead of recovering", jobID)
	}

	storedSecret, err := s.store.LoadSecret(jobID)
	if err != nil {
		return nil, fmt.Errorf("load job secret: %w", err)
	}
	secret := SecretInput{
		PostgresPassword:   storedSecret.PostgresPassword,
		ReplicatorPassword: storedSecret.ReplicatorPassword,
		AdminPassword:      storedSecret.AdminPassword,
		ExporterPassword:   storedSecret.ExporterPassword,
		DCSPassword:        storedSecret.DCSPassword,
	}
	if deployJob.Request.ControlPlaneDCS && secret.DCSPassword == "" {
		return nil, fmt.Errorf("job %s uses the shared control-plane DCS but its stored secret has no DCS password; the cluster cannot be recovered without it", jobID)
	}

	recoverySteps := s.recoveryStepsFor(deployJob.Request)
	recoveryJob := &Job{
		ID:                newJobID(),
		Status:            JobStatusRunning,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
		LastCompletedStep: -1,
		Request:           deployJob.Request,
		RecoveryOp:        &core.RecoveryOperation{SourceJobID: jobID},
		Steps:             make([]StepResult, 0, len(recoverySteps)),
	}
	s.updateJobProgress(recoveryJob)
	if err := s.store.Save(recoveryJob); err != nil {
		return nil, err
	}

	bgJob, err := s.store.Load(recoveryJob.ID)
	if err != nil {
		return nil, err
	}
	bgDeployJob, err := s.store.Load(jobID)
	if err != nil {
		return nil, err
	}
	s.start(func() {
		s.executeRecovery(s.ctx, bgJob, bgDeployJob, secret)
	})
	return recoveryJob, nil
}

// recoveryStepsFor returns the ordered Ansible steps for PostgreSQL post-outage
// recovery: cluster_bootstrap re-registers in DCS and starts Patroni; verify_cluster
// confirms the cluster is healthy. Both are safe to re-run on existing data.
//
// Clusters on the shared control plane re-provision their DCS tenant first.
// That is not ceremony: start/recover is the operation that follows a scale
// flow, which recreates node VMs from a stock image — the new node has no CA
// for the control plane and the control plane has no firewall grant for its
// address, both of which this step restores before Patroni is asked to start.
func (s *Service) recoveryStepsFor(spec StoredSpec) []step {
	steps := make([]step, 0, 4)
	if spec.ControlPlaneDCS {
		steps = append(steps, step{Name: controlPlaneDCSStep, Tag: controlPlaneDCSStep})
	}
	return append(steps,
		step{Name: "cluster_bootstrap", Tag: "cluster_bootstrap"},
		step{Name: "verify_cluster", Tag: "verify_cluster"},
		// Same gap add_member had: nothing else installs the exporter unit, so
		// a node that a scale operation rebuilt from a stock image reaches this
		// point healthy and then refuses every metrics scrape. The role is
		// idempotent and a no-op on nodes that already run it.
		step{Name: "setup_exporters", Tag: "setup_exporters"},
	)
}

/**
 * executeRecovery runs the recovery steps for a post-outage recovery job.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   recoveryJob *Job - the new recovery job being tracked
 *   deployJob *Job - the original deploy job supplying the cluster configuration
 *   secret SecretInput - the credentials to pass to Ansible
 */
func (s *Service) executeRecovery(ctx context.Context, recoveryJob *Job, deployJob *Job, secret SecretInput) {
	timeout := time.Duration(deployJob.Request.StepTimeoutSeconds) * time.Second
	recoverySteps := s.recoveryStepsFor(deployJob.Request)

	for i, st := range recoverySteps {
		recoveryJob.CurrentStep = st.Name
		s.updateJobProgress(recoveryJob)
		_ = s.store.Save(recoveryJob)

		cfg := runConfig{
			jobID:   deployJob.ID,
			spec:    deployJob.Request,
			secret:  secret,
			step:    st,
			timeout: execTimeoutForTag(st.Tag, timeout),
		}
		res := s.runDeploy(ctx, cfg)

		// A vertical scale stops the VM to resize it, and the cluster restart
		// that follows a scale is routinely issued before CloudStack has
		// finished — so a node goes down underneath a running step and Ansible
		// reports only UNREACHABLE (pam_nologin: "System is going down").
		//
		// That is a race, not a broken cluster, and erawan cannot lock against
		// it: CloudStack VM operations never reach the member-op lock. What it
		// can do is not fail a restart that merely arrived early. Every
		// recovery step is idempotent by design, so wait for the node to come
		// back and run the step once more.
		//
		// Deliberately narrow: only the shutdown signature retries, only once,
		// and only on the recovery path. An ordinary task failure, or an
		// UNREACHABLE from bad credentials, still fails immediately.
		if res.Status != JobStatusCompleted &&
			core.ExplainAnsibleFailure(res.Stdout+res.Stderr) != "" {
			nodes := append([]string{deployJob.Request.PrimaryIP}, deployJob.Request.StandbyIPs...)
			if err := core.WaitForSSH(ctx, nodes, deployJob.Request.SSHPort, scaleRebootWait); err == nil {
				res = s.runDeploy(ctx, cfg)
			}
		}
		recoveryJob.Steps = append(recoveryJob.Steps, res)

		if res.Status != JobStatusCompleted {
			recoveryJob.Status = JobStatusFailed
			recoveryJob.Error = res.Message
			if recoveryJob.Error == "" {
				recoveryJob.Error = fmt.Sprintf("recovery step %s failed", st.Name)
			}
			recoveryJob.LastCompletedStep = i - 1
			recoveryJob.CurrentStep = ""
			s.updateJobProgress(recoveryJob)
			_ = s.store.Save(recoveryJob)
			return
		}
		recoveryJob.LastCompletedStep = i
		recoveryJob.Error = ""
	}

	recoveryJob.Status = JobStatusCompleted
	recoveryJob.CurrentStep = ""
	recoveryJob.Error = ""
	s.updateJobProgress(recoveryJob)
	_ = s.store.Save(recoveryJob)
}

/**
 * Stop launches a graceful, data-preserving shutdown of the cluster owned by
 * jobID: Patroni is stopped on standbys first, then the primary (a clean
 * PostgreSQL shutdown), then etcd on every node. Data directories are never
 * touched; bring the cluster back with Recover (the start operation).
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   jobID string - ID of the deploy job whose cluster should be stopped
 *
 * Returns:
 *   *Job - the new running stop job
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) Stop(ctx context.Context, jobID string) (*Job, error) {
	_ = ctx
	deployJob, err := s.store.Load(jobID)
	if err != nil {
		return nil, fmt.Errorf("load job %q: %w", jobID, err)
	}
	if err := s.hydrateStoredSSHConfig(deployJob); err != nil {
		return nil, err
	}
	if deployJob.Status == core.JobStatusRolledBack {
		return nil, fmt.Errorf("job %s was rolled back; there is no cluster to stop", jobID)
	}

	stopJobID := newJobID()
	// Reuse the member-operation claim: a stop racing an add/remove-member or
	// another stop against the same cluster mutates shared service state, so
	// the second caller is rejected up front (and a Running deploy is too).
	if err := s.claimMemberOpLock(jobID, stopJobID); err != nil {
		return nil, err
	}

	stopJob := &Job{
		ID:                stopJobID,
		Status:            JobStatusRunning,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
		LastCompletedStep: -1,
		Request:           deployJob.Request,
		ServiceOp:         &core.ServiceOperation{Type: "stop", SourceJobID: jobID},
		Steps:             make([]StepResult, 0, 1),
	}
	s.updateJobProgress(stopJob)
	if err := s.store.Save(stopJob); err != nil {
		s.releaseMemberOpLock(jobID, stopJobID)
		return nil, err
	}

	bgStopJob, err := s.store.Load(stopJob.ID)
	if err != nil {
		s.releaseMemberOpLock(jobID, stopJobID)
		return nil, err
	}
	bgDeployJob, err := s.store.Load(jobID)
	if err != nil {
		s.releaseMemberOpLock(jobID, stopJobID)
		return nil, err
	}
	s.start(func() {
		defer s.releaseMemberOpLock(jobID, stopJobID)
		s.executeStop(s.ctx, bgStopJob, bgDeployJob)
	})
	return stopJob, nil
}

/**
 * executeStop runs the stop playbook as a single step and records the result.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   stopJob *Job - the stop job being tracked
 *   deployJob *Job - the original deploy job supplying the cluster configuration
 */
func (s *Service) executeStop(ctx context.Context, stopJob *Job, deployJob *Job) {
	timeout := time.Duration(deployJob.Request.StepTimeoutSeconds) * time.Second
	st := step{Name: "stop_cluster"}

	stopJob.CurrentStep = st.Name
	s.updateJobProgress(stopJob)
	_ = s.store.Save(stopJob)

	var res StepResult
	if s.runStopStep == nil {
		res = StepResult{
			Name:      st.Name,
			Status:    JobStatusFailed,
			StartedAt: time.Now().UTC(),
			EndedAt:   time.Now().UTC(),
			ExitCode:  -1,
			Message:   "stop runner is not configured",
		}
	} else {
		// Best-effort: the stop playbook needs no password, but it is told
		// which DCS layout this cluster uses so it does not go looking for a
		// local etcd that was never installed. A secret that fails to load is
		// not worth failing a stop over.
		storedSecret, _ := s.store.LoadSecret(deployJob.ID)
		res = s.runStopStep(ctx, runConfig{
			jobID:   deployJob.ID,
			spec:    deployJob.Request,
			secret:  SecretInput{DCSPassword: storedSecret.DCSPassword},
			step:    st,
			timeout: timeout,
		})
	}
	stopJob.Steps = append(stopJob.Steps, res)
	stopJob.CurrentStep = ""

	if res.Status != JobStatusCompleted {
		stopJob.Status = JobStatusFailed
		stopJob.Error = res.Message
		if stopJob.Error == "" {
			stopJob.Error = "stop cluster failed"
		}
	} else {
		stopJob.Status = JobStatusCompleted
		stopJob.LastCompletedStep = 0
		stopJob.Error = ""
	}
	s.updateJobProgress(stopJob)
	_ = s.store.Save(stopJob)
}

/**
 * claimMemberOpLock atomically marks deployJobID as owned by memberJobID for
 * the duration of a member operation. Two add/remove-member calls racing
 * against the same cluster both mutate etcd/Patroni membership (e.g.
 * overlapping learner promotions), which can transiently break quorum on the
 * primary — see core.Job.ActiveMemberJobID. Rejecting the second caller up
 * front surfaces a clear error immediately instead of letting it fail deep
 * inside Ansible a minute later.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   deployJobID string - the deploy job whose cluster is being claimed
 *   memberJobID string - the member job claiming it
 *
 * Returns:
 *   error - non-nil if the deploy job is still running or another member
 *     operation already holds the claim
 */
func (s *Service) claimMemberOpLock(deployJobID, memberJobID string) error {
	return s.store.Update(deployJobID, func(j *Job) error {
		if j.Status == JobStatusRunning {
			return fmt.Errorf("deploy job %s is still running; wait for it to finish before starting a member operation", deployJobID)
		}
		if j.ActiveMemberJobID != "" {
			return fmt.Errorf("another member operation (%s) is already running against job %s; wait for it to finish first", j.ActiveMemberJobID, deployJobID)
		}
		j.ActiveMemberJobID = memberJobID
		return nil
	})
}

/**
 * releaseMemberOpLock clears deployJobID's claim if it is still held by
 * memberJobID. Safe to call multiple times or after a failed claim.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   deployJobID string - the deploy job to release
 *   memberJobID string - the member job releasing it
 */
func (s *Service) releaseMemberOpLock(deployJobID, memberJobID string) {
	_ = s.store.Update(deployJobID, func(j *Job) error {
		if j.ActiveMemberJobID == memberJobID {
			j.ActiveMemberJobID = ""
		}
		return nil
	})
}

/**
 * AddMember.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   req AddMemberRequest - the req (AddMemberRequest)
 *
 * Returns:
 *   *Job - the resulting *Job
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) AddMember(ctx context.Context, req AddMemberRequest) (*Job, error) {
	_ = ctx
	if err := ValidateAddMemberRequest(&req); err != nil {
		return nil, err
	}
	deployJob, err := s.store.Load(req.JobID)
	if err != nil {
		return nil, fmt.Errorf("load job %q: %w", req.JobID, err)
	}
	if err := s.hydrateStoredSSHConfig(deployJob); err != nil {
		return nil, err
	}

	existing := make(map[string]struct{}, len(deployJob.Request.StandbyIPs)+1)
	existing[deployJob.Request.PrimaryIP] = struct{}{}
	for _, ip := range deployJob.Request.StandbyIPs {
		existing[ip] = struct{}{}
	}
	for _, ip := range req.MemberIPs {
		if _, ok := existing[ip]; ok {
			return nil, fmt.Errorf("member_ip %s is already in the cluster", ip)
		}
	}

	if _, err := s.store.LoadSecret(req.JobID); err != nil {
		return nil, fmt.Errorf("load job secret %q: %w", req.JobID, err)
	}

	memberJobID := newJobID()
	if err := s.claimMemberOpLock(req.JobID, memberJobID); err != nil {
		return nil, err
	}

	memberJob := &Job{
		ID:                memberJobID,
		Status:            JobStatusRunning,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
		LastCompletedStep: -1,
		Request:           deployJob.Request,
		MemberOp: &MemberOperation{
			Type:        "add",
			MemberIPs:   req.MemberIPs,
			SourceJobID: req.JobID,
		},
		Steps: make([]StepResult, 0, len(req.MemberIPs)),
	}
	s.updateJobProgress(memberJob)
	if err := s.store.Save(memberJob); err != nil {
		s.releaseMemberOpLock(req.JobID, memberJobID)
		return nil, err
	}

	bgMemberJob, err := s.store.Load(memberJob.ID)
	if err != nil {
		s.releaseMemberOpLock(req.JobID, memberJobID)
		return nil, err
	}
	bgDeployJob, err := s.store.Load(req.JobID)
	if err != nil {
		s.releaseMemberOpLock(req.JobID, memberJobID)
		return nil, err
	}
	resetHostKeys := req.ResetHostKeys
	s.start(func() {
		defer s.releaseMemberOpLock(req.JobID, memberJobID)
		s.executeMemberAdd(s.ctx, bgMemberJob, bgDeployJob, resetHostKeys)
	})
	return memberJob, nil
}

/**
 * RemoveMember.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   req RemoveMemberRequest - the req (RemoveMemberRequest)
 *
 * Returns:
 *   *Job - the resulting *Job
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) RemoveMember(ctx context.Context, req RemoveMemberRequest) (*Job, error) {
	_ = ctx
	if err := ValidateRemoveMemberRequest(&req); err != nil {
		return nil, err
	}
	deployJob, err := s.store.Load(req.JobID)
	if err != nil {
		return nil, fmt.Errorf("load job %q: %w", req.JobID, err)
	}
	if err := s.hydrateStoredSSHConfig(deployJob); err != nil {
		return nil, err
	}

	if deployJob.Request.PrimaryIP == req.MemberIP {
		return nil, fmt.Errorf("cannot remove the primary node %s; promote a standby first", req.MemberIP)
	}
	found := false
	for _, ip := range deployJob.Request.StandbyIPs {
		if ip == req.MemberIP {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("member_ip %s is not in the cluster", req.MemberIP)
	}

	if _, err := s.store.LoadSecret(req.JobID); err != nil {
		return nil, fmt.Errorf("load job secret %q: %w", req.JobID, err)
	}

	memberJobID := newJobID()
	if err := s.claimMemberOpLock(req.JobID, memberJobID); err != nil {
		return nil, err
	}

	memberJob := &Job{
		ID:                memberJobID,
		Status:            JobStatusRunning,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
		LastCompletedStep: -1,
		Request:           deployJob.Request,
		MemberOp: &MemberOperation{
			Type:        "remove",
			MemberIPs:   []string{req.MemberIP},
			SourceJobID: req.JobID,
		},
		Steps: make([]StepResult, 0, 1),
	}
	s.updateJobProgress(memberJob)
	if err := s.store.Save(memberJob); err != nil {
		s.releaseMemberOpLock(req.JobID, memberJobID)
		return nil, err
	}

	bgMemberJob, err := s.store.Load(memberJob.ID)
	if err != nil {
		s.releaseMemberOpLock(req.JobID, memberJobID)
		return nil, err
	}
	bgDeployJob, err := s.store.Load(req.JobID)
	if err != nil {
		s.releaseMemberOpLock(req.JobID, memberJobID)
		return nil, err
	}
	force := req.Force
	s.start(func() {
		defer s.releaseMemberOpLock(req.JobID, memberJobID)
		s.executeMemberRemove(s.ctx, bgMemberJob, bgDeployJob, force)
	})
	return memberJob, nil
}

/**
 * executeMemberAdd.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   memberJob *Job - the memberJob (*Job)
 *   deployJob *Job - the deployJob (*Job)
 *   resetHostKeys bool - when true, forget any pinned SSH host key for the
 *     new members before connecting
 */
func (s *Service) executeMemberAdd(ctx context.Context, memberJob *Job, deployJob *Job, resetHostKeys bool) {
	storedSecret, err := s.store.LoadSecret(deployJob.ID)
	if err != nil {
		memberJob.Status = JobStatusFailed
		memberJob.Error = fmt.Sprintf("load job secret: %s", err)
		s.updateJobProgress(memberJob)
		_ = s.store.Save(memberJob)
		return
	}
	secret := SecretInput{
		PostgresPassword:   storedSecret.PostgresPassword,
		ReplicatorPassword: storedSecret.ReplicatorPassword,
		AdminPassword:      storedSecret.AdminPassword,
		DCSPassword:        storedSecret.DCSPassword,
	}
	timeout := time.Duration(deployJob.Request.StepTimeoutSeconds) * time.Second

	// Members are added strictly one at a time: every addition mutates etcd
	// membership on the primary (etcd allows a single unpromoted learner by
	// default), and each run's register play treats members outside its
	// standby list as stale registrations to clean up — so a sibling still
	// mid-join would be seen as removable. Appending each success to
	// StandbyIPs before the next run makes it a legitimate member for the
	// runs that follow.
	for _, ip := range memberJob.MemberOp.MemberIPs {
		memberJob.CurrentStep = ip
		s.updateJobProgress(memberJob)
		_ = s.store.Save(memberJob)

		// The new node must trust the control plane and be allowed through its
		// firewall before Patroni starts there, and the tenant's etcd user has
		// to exist (it may have been dropped out of band). Provisioning is
		// idempotent, so this is also the repair path for the existing nodes.
		if deployJob.Request.ControlPlaneDCS {
			dcsResult := s.runDCSProvisionForMember(ctx, deployJob, secret, ip, resetHostKeys)
			memberJob.Steps = append(memberJob.Steps, dcsResult)
			if dcsResult.Status != JobStatusCompleted {
				memberJob.Status = JobStatusFailed
				memberJob.Error = dcsResult.Message
				if memberJob.Error == "" {
					memberJob.Error = fmt.Sprintf("control-plane DCS provisioning for %s failed", ip)
				}
				memberJob.CurrentStep = ""
				s.updateJobProgress(memberJob)
				_ = s.store.Save(memberJob)
				return
			}
		}

		result := s.doAddMember(ctx, memberRunConfig{
			jobID:         deployJob.ID,
			spec:          deployJob.Request,
			secret:        secret,
			memberIP:      ip,
			timeout:       timeout,
			resetHostKeys: resetHostKeys,
		})
		memberJob.Steps = append(memberJob.Steps, result)

		if result.Status == JobStatusCompleted {
			// Re-read and persist the deploy job under the store lock so a
			// concurrent member operation on the same cluster cannot clobber the
			// standby list.
			_ = s.store.Update(deployJob.ID, func(j *Job) error {
				j.Request.StandbyIPs = append(j.Request.StandbyIPs, ip)
				deployJob.Request.StandbyIPs = j.Request.StandbyIPs
				return nil
			})
			memberJob.Request.StandbyIPs = deployJob.Request.StandbyIPs
		} else {
			memberJob.Status = JobStatusFailed
			memberJob.Error = result.Message
			if memberJob.Error == "" {
				memberJob.Error = fmt.Sprintf("add member %s failed", ip)
			}
			memberJob.CurrentStep = ""
			s.updateJobProgress(memberJob)
			_ = s.store.Save(memberJob)
			return
		}
	}

	memberJob.Status = JobStatusCompleted
	memberJob.CurrentStep = ""
	memberJob.Error = ""
	s.updateJobProgress(memberJob)
	_ = s.store.Save(memberJob)
}

/**
 * executeMemberRemove.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   memberJob *Job - the memberJob (*Job)
 *   deployJob *Job - the deployJob (*Job)
 *   force bool - the force flag
 */
func (s *Service) executeMemberRemove(ctx context.Context, memberJob *Job, deployJob *Job, force bool) {
	storedSecret, err := s.store.LoadSecret(deployJob.ID)
	if err != nil {
		memberJob.Status = JobStatusFailed
		memberJob.Error = fmt.Sprintf("load job secret: %s", err)
		s.updateJobProgress(memberJob)
		_ = s.store.Save(memberJob)
		return
	}
	secret := SecretInput{
		PostgresPassword:   storedSecret.PostgresPassword,
		ReplicatorPassword: storedSecret.ReplicatorPassword,
		AdminPassword:      storedSecret.AdminPassword,
		DCSPassword:        storedSecret.DCSPassword,
	}
	timeout := time.Duration(deployJob.Request.StepTimeoutSeconds) * time.Second
	ip := memberJob.MemberOp.MemberIPs[0]

	memberJob.CurrentStep = ip
	s.updateJobProgress(memberJob)
	_ = s.store.Save(memberJob)

	result := s.doRemoveMember(ctx, memberRunConfig{
		jobID:    deployJob.ID,
		spec:     deployJob.Request,
		secret:   secret,
		memberIP: ip,
		force:    force,
		timeout:  timeout,
	})
	memberJob.Steps = append(memberJob.Steps, result)

	if result.Status == JobStatusCompleted {
		// Re-read and persist the deploy job under the store lock so a concurrent
		// member operation on the same cluster cannot clobber the standby list.
		_ = s.store.Update(deployJob.ID, func(j *Job) error {
			j.Request.StandbyIPs = without(j.Request.StandbyIPs, ip)
			deployJob.Request.StandbyIPs = j.Request.StandbyIPs
			return nil
		})
		memberJob.Request.StandbyIPs = deployJob.Request.StandbyIPs
		memberJob.Status = JobStatusCompleted
		memberJob.Error = ""

		// The removed node keeps its grant on the control plane's client port
		// until this runs, and CloudStack recycles IPs — the same reason the
		// remove_member playbook strips its pg_hba and UFW entries on the
		// remaining nodes. Reported as a step but not fatal: the member is
		// already out of the cluster, and the next operation converges it.
		if deployJob.Request.ControlPlaneDCS {
			dcsResult := s.runDCSRevokeForMember(ctx, deployJob, secret, ip)
			memberJob.Steps = append(memberJob.Steps, dcsResult)
		}
	} else {
		memberJob.Status = JobStatusFailed
		memberJob.Error = result.Message
		if memberJob.Error == "" {
			memberJob.Error = fmt.Sprintf("remove member %s failed", ip)
		}
	}
	memberJob.CurrentStep = ""
	s.updateJobProgress(memberJob)
	_ = s.store.Save(memberJob)
}

/**
 * runDCSProvisionForMember provisions the control-plane DCS ahead of an
 * add-member run: the joining node gets the control plane's CA and a firewall
 * grant, and the tenant's etcd user/role are converged.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   deployJob *Job - the deploy job supplying the cluster configuration
 *   secret SecretInput - the cluster's secrets, including the DCS password
 *   memberIP string - the node being added
 *   resetHostKeys bool - forget any pinned host key for the new node first
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (s *Service) runDCSProvisionForMember(ctx context.Context, deployJob *Job, secret SecretInput, memberIP string, resetHostKeys bool) StepResult {
	st := step{Name: controlPlaneDCSStep}
	if s.runDCSProvision == nil {
		return StepResult{
			Name:      st.Name,
			Status:    JobStatusFailed,
			StartedAt: time.Now().UTC(),
			EndedAt:   time.Now().UTC(),
			ExitCode:  -1,
			Message:   "control-plane DCS runner is not configured",
		}
	}
	return s.runDCSProvision(ctx, dcsRunConfig{
		jobID:  deployJob.ID,
		spec:   deployJob.Request,
		secret: secret,
		// Only the joining node: the existing nodes are serving traffic and
		// already hold everything this installs.
		clientIPs:     []string{memberIP},
		step:          st,
		timeout:       dcsExecTimeout,
		resetHostKeys: resetHostKeys,
	})
}

/**
 * runDCSRevokeForMember drops a removed node's access to the control plane. It
 * contacts the control plane only — the removed node is stopped by then.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   deployJob *Job - the deploy job supplying the cluster configuration
 *   secret SecretInput - the cluster's secrets, including the DCS password
 *   memberIP string - the node that was removed
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (s *Service) runDCSRevokeForMember(ctx context.Context, deployJob *Job, secret SecretInput, memberIP string) StepResult {
	st := step{Name: "control_plane_dcs_revoke"}
	if s.runDCSProvision == nil {
		return StepResult{
			Name:      st.Name,
			Status:    JobStatusFailed,
			StartedAt: time.Now().UTC(),
			EndedAt:   time.Now().UTC(),
			ExitCode:  -1,
			Message:   "control-plane DCS runner is not configured",
		}
	}
	return s.runDCSProvision(ctx, dcsRunConfig{
		jobID:     deployJob.ID,
		spec:      deployJob.Request,
		secret:    secret,
		revokeIPs: []string{memberIP},
		step:      st,
		timeout:   dcsExecTimeout,
	})
}

/**
 * doAddMember.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg memberRunConfig - the cfg (memberRunConfig)
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (s *Service) doAddMember(ctx context.Context, cfg memberRunConfig) StepResult {
	if s.runAddMemberStep == nil {
		return StepResult{
			Name:      "add_member",
			Status:    JobStatusFailed,
			StartedAt: time.Now().UTC(),
			EndedAt:   time.Now().UTC(),
			ExitCode:  -1,
			Message:   "add member runner is not configured",
		}
	}
	return s.runAddMemberStep(ctx, cfg)
}

/**
 * doRemoveMember.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg memberRunConfig - the cfg (memberRunConfig)
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (s *Service) doRemoveMember(ctx context.Context, cfg memberRunConfig) StepResult {
	if s.runRemMemberStep == nil {
		return StepResult{
			Name:      "remove_member",
			Status:    JobStatusFailed,
			StartedAt: time.Now().UTC(),
			EndedAt:   time.Now().UTC(),
			ExitCode:  -1,
			Message:   "remove member runner is not configured",
		}
	}
	return s.runRemMemberStep(ctx, cfg)
}

/**
 * Get.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   jobID string - the jobID string
 *
 * Returns:
 *   *Job - the resulting *Job
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) Get(jobID string) (*Job, error) {
	job, err := s.store.Load(jobID)
	if err != nil {
		return nil, err
	}
	s.updateJobProgress(job)
	return job, nil
}

/**
 * GetSecret.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   jobID string - the jobID string
 *
 * Returns:
 *   *StoredSecret - the resulting *StoredSecret
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) GetSecret(jobID string) (*StoredSecret, error) {
	secret, err := s.store.LoadSecret(jobID)
	if err != nil {
		return nil, err
	}
	return &secret, nil
}

/**
 * List.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   limit int - the limit value
 *
 * Returns:
 *   []Job - the resulting []Job
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) List(limit int) ([]Job, error) {
	jobs, err := s.store.List(limit)
	if err != nil {
		return nil, err
	}
	for i := range jobs {
		s.updateJobProgress(&jobs[i])
	}
	return jobs, nil
}

/**
 * hydrateStoredSSHConfig.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   job *Job - the job (*Job)
 *
 * Returns:
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) hydrateStoredSSHConfig(job *Job) error {
	if s.sshUser == "" || s.sshKeyPath == "" {
		return fmt.Errorf("ssh service configuration is incomplete")
	}
	if job != nil {
		if job.Request.SSHUser == "" {
			job.Request.SSHUser = s.sshUser
		}
		if job.Request.SSHPrivateKeyPath == "" {
			job.Request.SSHPrivateKeyPath = s.sshKeyPath
		}
	}
	return nil
}

/**
 * executeFrom.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   job *Job - the job (*Job)
 *   startIndex int - the startIndex value
 *   secret SecretInput - the secret (SecretInput)
 *   resetHostKeys bool - when true, forget any pinned SSH host key for this
 *     cluster's nodes before connecting, so a rebuilt/reimaged node's new key
 *     is trusted instead of failing host-key verification
 *
 * Returns:
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) executeFrom(ctx context.Context, job *Job, startIndex int, secret SecretInput, resetHostKeys bool) error {
	timeout := time.Duration(job.Request.StepTimeoutSeconds) * time.Second
	for i := startIndex; i < len(s.steps); i++ {
		st := s.steps[i]
		if reason, shouldSkip := shouldSkipStep(st, job.Request); shouldSkip {
			job.LastCompletedStep = i
			job.CurrentStep = st.Name
			job.Steps = append(job.Steps, StepResult{
				Name:      st.Name,
				Status:    "skipped",
				StartedAt: time.Now().UTC(),
				EndedAt:   time.Now().UTC(),
				ExitCode:  0,
				Message:   reason,
			})
			s.updateJobProgress(job)
			if err := s.store.Save(job); err != nil {
				return err
			}
			continue
		}

		job.CurrentStep = st.Name
		s.updateJobProgress(job)
		if err := s.store.Save(job); err != nil {
			return err
		}

		res := s.runDeploy(ctx, runConfig{
			jobID:  job.ID,
			spec:   job.Request,
			secret: secret,
			step:   st,
			// This is the deploy pipeline (a first run or a resume of one), the
			// only caller allowed to clear an orphaned tenant prefix on the
			// control plane. executeRecovery deliberately leaves it false.
			freshDeploy:   true,
			timeout:       execTimeoutForTag(st.Tag, timeout),
			resetHostKeys: resetHostKeys,
		})
		job.Steps = append(job.Steps, res)

		if res.Status != JobStatusCompleted {
			job.Status = JobStatusFailed
			job.Error = res.Message
			if job.Error == "" {
				job.Error = fmt.Sprintf("step %s failed", st.Name)
			}
			s.updateJobProgress(job)
			_ = s.store.Save(job)
			return fmt.Errorf("%s", job.Error)
		}

		job.LastCompletedStep = i
		job.Error = ""
		s.updateJobProgress(job)
		if err := s.store.Save(job); err != nil {
			return err
		}
	}

	job.Status = JobStatusCompleted
	job.CurrentStep = ""
	job.Error = ""
	s.updateJobProgress(job)
	if err := s.store.Save(job); err != nil {
		return err
	}
	return nil
}

/**
 * runDeploy.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg runConfig - the cfg (runConfig)
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (s *Service) runDeploy(ctx context.Context, cfg runConfig) StepResult {
	// The control-plane step is not a tag of the deploy playbook — it runs the
	// shared DCS playbook against its own inventory. Dispatching here covers
	// both callers (deploy/resume via executeFrom, start/recover via
	// executeRecovery) in one place.
	if cfg.step.Name == controlPlaneDCSStep {
		return s.runDCSProvisionFor(ctx, cfg)
	}
	if s.runDeployStep == nil {
		return StepResult{
			Name:      cfg.step.Name,
			Status:    JobStatusFailed,
			StartedAt: time.Now().UTC(),
			EndedAt:   time.Now().UTC(),
			ExitCode:  -1,
			Message:   "deploy runner is not configured",
		}
	}
	return s.runDeployStep(ctx, cfg)
}

/**
 * runDCSProvisionFor runs the shared control-plane provisioning playbook for a
 * deploy/recovery step, targeting every node of the cluster.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg runConfig - the deploy step being executed
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (s *Service) runDCSProvisionFor(ctx context.Context, cfg runConfig) StepResult {
	if s.runDCSProvision == nil {
		return StepResult{
			Name:      cfg.step.Name,
			Status:    JobStatusFailed,
			StartedAt: time.Now().UTC(),
			EndedAt:   time.Now().UTC(),
			ExitCode:  -1,
			Message:   "control-plane DCS runner is not configured",
		}
	}
	return s.runDCSProvision(ctx, dcsRunConfig{
		jobID:         cfg.jobID,
		spec:          cfg.spec,
		secret:        cfg.secret,
		clientIPs:     append([]string{cfg.spec.PrimaryIP}, cfg.spec.StandbyIPs...),
		step:          cfg.step,
		timeout:       cfg.timeout,
		freshDeploy:   cfg.freshDeploy,
		resetHostKeys: cfg.resetHostKeys,
	})
}

/**
 * ReleaseDCS launches a job that releases this cluster's namespace on the
 * shared control-plane etcd: its keys, its user, its role, and its nodes'
 * access to the control plane's client port.
 *
 * Call it when a cluster is decommissioned. Nothing else does: the tenant's
 * objects are named after the cluster, so leaving them behind both accumulates
 * state on infrastructure every tenant shares and hands a later cluster of the
 * same name someone else's leftover keys. It runs against the control plane
 * only, so the cluster's own VMs may already be gone.
 *
 * The cluster's data is untouched — this removes coordination state, not
 * PostgreSQL data — but a cluster whose nodes are still running WILL lose its
 * DCS and stop electing a leader, so it is rejected while another operation on
 * the cluster is in flight.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   jobID string - ID of the deploy job whose cluster should be released
 *
 * Returns:
 *   *Job - the new running release job
 *   error - error value; non-nil when the operation fails
 */
func (s *Service) ReleaseDCS(ctx context.Context, jobID string) (*Job, error) {
	_ = ctx
	deployJob, err := s.store.Load(jobID)
	if err != nil {
		return nil, fmt.Errorf("load job %q: %w", jobID, err)
	}
	if !deployJob.Request.ControlPlaneDCS {
		return nil, fmt.Errorf("job %s keeps its DCS on its own nodes; there is nothing to release on the control plane", jobID)
	}
	if err := s.hydrateStoredSSHConfig(deployJob); err != nil {
		return nil, err
	}

	releaseJobID := newJobID()
	// Same claim as stop/add/remove-member: this mutates cluster-wide state, so
	// it must not race another operation against the same cluster.
	if err := s.claimMemberOpLock(jobID, releaseJobID); err != nil {
		return nil, err
	}

	releaseJob := &Job{
		ID:                releaseJobID,
		Status:            JobStatusRunning,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
		LastCompletedStep: -1,
		Request:           deployJob.Request,
		ServiceOp:         &core.ServiceOperation{Type: "release_dcs", SourceJobID: jobID},
		Steps:             make([]StepResult, 0, 1),
	}
	s.updateJobProgress(releaseJob)
	if err := s.store.Save(releaseJob); err != nil {
		s.releaseMemberOpLock(jobID, releaseJobID)
		return nil, err
	}

	bgReleaseJob, err := s.store.Load(releaseJob.ID)
	if err != nil {
		s.releaseMemberOpLock(jobID, releaseJobID)
		return nil, err
	}
	bgDeployJob, err := s.store.Load(jobID)
	if err != nil {
		s.releaseMemberOpLock(jobID, releaseJobID)
		return nil, err
	}
	s.start(func() {
		defer s.releaseMemberOpLock(jobID, releaseJobID)
		s.executeReleaseDCS(s.ctx, bgReleaseJob, bgDeployJob)
	})
	return releaseJob, nil
}

/**
 * executeReleaseDCS runs the control-plane cleanup playbook as a single step.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   releaseJob *Job - the release job being tracked
 *   deployJob *Job - the deploy job supplying the cluster configuration
 */
func (s *Service) executeReleaseDCS(ctx context.Context, releaseJob *Job, deployJob *Job) {
	st := step{Name: "release_control_plane_dcs"}
	releaseJob.CurrentStep = st.Name
	s.updateJobProgress(releaseJob)
	_ = s.store.Save(releaseJob)

	var res StepResult
	if s.runDCSCleanup == nil {
		res = StepResult{
			Name:      st.Name,
			Status:    JobStatusFailed,
			StartedAt: time.Now().UTC(),
			EndedAt:   time.Now().UTC(),
			ExitCode:  -1,
			Message:   "control-plane DCS runner is not configured",
		}
	} else {
		res = s.runDCSCleanup(ctx, dcsRunConfig{
			jobID: deployJob.ID,
			spec:  deployJob.Request,
			// The cluster's nodes are not contacted: they are usually already
			// destroyed. They are still listed so their grant on the control
			// plane's client port goes away with the tenant.
			revokeIPs: append([]string{deployJob.Request.PrimaryIP}, deployJob.Request.StandbyIPs...),
			step:      st,
			timeout:   dcsExecTimeout,
		})
	}
	releaseJob.Steps = append(releaseJob.Steps, res)
	releaseJob.CurrentStep = ""

	if res.Status != JobStatusCompleted {
		releaseJob.Status = JobStatusFailed
		releaseJob.Error = res.Message
		if releaseJob.Error == "" {
			releaseJob.Error = "release control-plane DCS failed"
		}
	} else {
		releaseJob.Status = JobStatusCompleted
		releaseJob.LastCompletedStep = 0
		releaseJob.Error = ""
	}
	s.updateJobProgress(releaseJob)
	_ = s.store.Save(releaseJob)
}

/**
 * CollectMetrics.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   req MetricRequest - the req (MetricRequest)
 *
 * Returns:
 *   MetricResponse - the resulting MetricResponse
 */
func (s *Service) CollectMetrics(ctx context.Context, req MetricRequest) MetricResponse {
	return s.collector.Collect(ctx, req)
}

/**
 * ConnectionInfo.
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
 *   nodeIPs []string - the nodeIPs ([]string)
 *   err error - error value; non-nil when the operation fails
 */
func (s *Service) ConnectionInfo(ctx context.Context, jobID string) (host string, port int, user, password string, nodeIPs []string, err error) {
	job, err := s.store.Load(jobID)
	if err != nil {
		return "", 0, "", "", nil, fmt.Errorf("load job %q: %w", jobID, err)
	}
	secret, err := s.store.LoadSecret(jobID)
	if err != nil {
		return "", 0, "", "", nil, fmt.Errorf("load job secret %q: %w", jobID, err)
	}
	p := job.Request.PostgresPort
	if p == 0 {
		p = 5432
	}
	ips := append([]string{job.Request.PrimaryIP}, job.Request.StandbyIPs...)
	return job.Request.PrimaryIP, p, secret.PostgresUser, secret.PostgresPassword, ips, nil
}

/**
 * updateJobProgress.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   job *Job - the job (*Job)
 */
func (s *Service) updateJobProgress(job *Job) {
	total := s.totalStepsFor(job.Request)
	if job.MemberOp != nil {
		total = len(job.MemberOp.MemberIPs)
	}
	if job.RecoveryOp != nil {
		total = len(s.recoveryStepsFor(job.Request))
	}
	if job.ServiceOp != nil {
		total = 1
	}
	core.ApplyProgress(job, total)
}

/**
 * totalStepsFor.
 *
 * Receiver:
 *   s *Service - pointer receiver; the method may mutate this Service instance
 *
 * Params:
 *   spec StoredSpec - the spec (StoredSpec)
 *
 * Returns:
 *   int - the resulting integer
 */
func (s *Service) totalStepsFor(spec StoredSpec) int {
	total := 0
	for _, st := range s.steps {
		if _, skip := shouldSkipStep(st, spec); skip {
			continue
		}
		total++
	}
	return total
}

/**
 * shouldSkipStep.
 *
 * Params:
 *   st step - the st (step)
 *   spec StoredSpec - the spec (StoredSpec)
 *
 * Returns:
 *   string - the resulting string
 *   bool - boolean result
 */
func shouldSkipStep(st step, spec StoredSpec) (string, bool) {
	if st.Name == controlPlaneDCSStep && !spec.ControlPlaneDCS {
		return "cluster keeps its DCS on its own nodes", true
	}
	if st.Name == "standby_config" && len(spec.StandbyIPs) == 0 {
		return "standby_ips is empty", true
	}
	if st.Skippable && (spec.NewUser == "" || spec.NewDB == "") {
		return "new_user/new_db not provided", true
	}
	return "", false
}

/**
 * without returns slice with every occurrence of target removed.
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

/**
 * newJobID.
 *
 * Returns:
 *   string - the resulting string
 */
func newJobID() string { return core.NewJobID() }

/**
 * stringOrGenerated.
 *
 * Params:
 *   value string - the value string
 *
 * Returns:
 *   string - the resulting string
 */
func stringOrGenerated(value string) string { return core.OrRandomSecret(value) }
