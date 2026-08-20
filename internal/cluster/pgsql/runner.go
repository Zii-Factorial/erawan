package pgsql

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"erawan-cluster/internal/cluster/core"
)

const (
	defaultPostgreSQLCluster = "main"
	defaultPostgresSuperuser = "postgres"
	defaultReplicationUser   = "replicator"
	defaultPatroniAdminUser  = "admin"
)

type Runner struct {
	ansibleBin           string
	deployPlaybook       string
	addMemberPlaybook    string
	removeMemberPlaybook string
	stopPlaybook         string
	ansibleVerbosity     int
	streamLogs           bool
	maxOutputChars       int
	sshPolicy            core.SSHPolicy
	proxyAllowedIP       string
	controlPlane         core.ControlPlane
}

/**
 * NewRunner.
 *
 * Params:
 *   ansibleBin string - the ansibleBin string
 *   deployPlaybook string - the deployPlaybook string
 *
 * Returns:
 *   *Runner - the resulting *Runner
 */
func NewRunner(ansibleBin, deployPlaybook string) *Runner {
	if strings.TrimSpace(ansibleBin) == "" {
		ansibleBin = "ansible-playbook"
	}
	return &Runner{
		ansibleBin:     ansibleBin,
		deployPlaybook: deployPlaybook,
		maxOutputChars: 8000,
		// Secure by default: verify node SSH host keys.
		sshPolicy: core.SSHPolicy{VerifyHostKeys: true},
	}
}

/**
 * SetAddMemberPlaybook.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   path string - the path string
 */
func (r *Runner) SetAddMemberPlaybook(path string) { r.addMemberPlaybook = path }

/**
 * SetRemoveMemberPlaybook.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   path string - the path string
 */
func (r *Runner) SetRemoveMemberPlaybook(path string) { r.removeMemberPlaybook = path }

/**
 * SetStopPlaybook.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   path string - the path string
 */
func (r *Runner) SetStopPlaybook(path string) { r.stopPlaybook = path }

/**
 * SetSSHPolicy configures how Ansible verifies node SSH host keys.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   p core.SSHPolicy - the p (core.SSHPolicy)
 */
func (r *Runner) SetSSHPolicy(p core.SSHPolicy) { r.sshPolicy = p }

/**
 * SetProxyAllowedIP configures the single source IP (the HAProxy/control-plane
 * node) permitted to reach client-facing DB ports and accounts. Cluster-internal
 * node-to-node access (replication, Patroni/etcd coordination) is scoped
 * separately by peer IP, not by this value.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ip string - the allowed proxy IP
 */
func (r *Runner) SetProxyAllowedIP(ip string) { r.proxyAllowedIP = ip }

/**
 * SetControlPlane configures the shared control plane that carries the Patroni
 * DCS. When it is enabled, clusters deployed from here keep no etcd of their
 * own: Patroni talks to the control plane's etcd over TLS with a per-tenant
 * user, which is what makes a one- or two-node PostgreSQL cluster viable.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   cp core.ControlPlane - the resolved control-plane configuration
 */
func (r *Runner) SetControlPlane(cp core.ControlPlane) { r.controlPlane = cp }

/**
 * ControlPlaneEnabled reports whether new deploys should place their DCS on the
 * shared control plane. Existing jobs are NOT re-evaluated against this: each
 * cluster records the mode it was deployed with (StoredSpec.ControlPlaneDCS),
 * so turning the environment variable on or off never re-points a running
 * cluster at a different DCS.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Returns:
 *   bool - true when a shared control plane is configured
 */
func (r *Runner) ControlPlaneEnabled() bool { return r.controlPlane.Enabled() }

/**
 * ControlPlaneTenantUser returns the etcd username a cluster is issued on the
 * shared control plane, so the service can record it alongside the password in
 * the job's stored secret.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   clusterName string - the cluster (tenant) name
 *
 * Returns:
 *   string - the resulting username, empty when no control plane is configured
 */
func (r *Runner) ControlPlaneTenantUser(clusterName string) string {
	if !r.controlPlane.Enabled() {
		return ""
	}
	return r.controlPlane.TenantUser(clusterName)
}

/**
 * controlPlaneVars returns the node-facing DCS settings for one cluster, or an
 * empty map when this cluster keeps its DCS on its own nodes. These are what
 * patroni.yml.j2 renders into the etcd3 section, plus the switch every
 * etcd-related task in the playbooks is gated on.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   spec StoredSpec - the cluster spec, which records its DCS mode
 *   dcsPassword string - the cluster's etcd password from its stored secret
 *
 * Returns:
 *   map[string]any - extra vars to merge into an engine playbook run
 */
func (r *Runner) controlPlaneVars(spec StoredSpec, dcsPassword string) map[string]any {
	if !r.usesControlPlane(spec) {
		return nil
	}
	vars := map[string]any{
		"control_plane_ip":               r.controlPlane.IP,
		"control_plane_etcd_client_port": r.controlPlane.EtcdClientPort,
		"control_plane_etcd_user":        r.controlPlane.TenantUser(spec.ClusterName),
		"control_plane_etcd_password":    dcsPassword,
		"patroni_etcd_ca_path":           r.controlPlane.NodeCAPath,
		// Patroni's key path is namespace + scope, and the tenant's etcd role
		// is granted exactly that prefix — so the namespace has to be the
		// control plane's, not the per-node default.
		"patroni_namespace": r.controlPlane.Namespace,
	}
	// Rendered into patroni.yml only when the tenant holds a client
	// certificate; a control plane running client-cert-auth refuses the TLS
	// handshake without one.
	if r.controlPlane.ClientCert {
		vars["patroni_etcd_client_cert_path"] = r.controlPlane.NodeCertPath
		vars["patroni_etcd_client_key_path"] = r.controlPlane.NodeKeyPath
	}
	return vars
}

/**
 * usesControlPlane reports whether a specific cluster's DCS lives on the shared
 * control plane. Both conditions must hold: the process must have a control
 * plane configured, and the cluster must have been deployed against one.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   spec StoredSpec - the cluster spec
 *
 * Returns:
 *   bool - true when this cluster's DCS is on the control plane
 */
func (r *Runner) usesControlPlane(spec StoredSpec) bool {
	return spec.ControlPlaneDCS && r.controlPlane.Enabled()
}

/**
 * SetDebug.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   verbosity int - the verbosity value
 *   streamLogs bool - the streamLogs flag
 *   maxOutputChars int - the maxOutputChars value
 */
func (r *Runner) SetDebug(verbosity int, streamLogs bool, maxOutputChars int) {
	if verbosity < 0 {
		verbosity = 0
	}
	r.ansibleVerbosity = verbosity
	r.streamLogs = streamLogs
	if maxOutputChars > 0 {
		r.maxOutputChars = maxOutputChars
	}
}

type runConfig struct {
	jobID   string
	spec    StoredSpec
	secret  SecretInput
	step    step
	timeout time.Duration
	// freshDeploy marks a run of the deploy pipeline (including a resume of
	// one) as opposed to start/recover. Only a deploy is entitled to clear
	// Patroni state left on the shared control plane by an earlier cluster of
	// the same name; a recover must keep the state its nodes' data belongs to.
	freshDeploy   bool
	resetHostKeys bool
}

type memberRunConfig struct {
	jobID         string
	spec          StoredSpec
	secret        SecretInput
	memberIP      string
	force         bool
	timeout       time.Duration
	resetHostKeys bool
}

// dcsRunConfig describes one run of a shared control-plane playbook. clientIPs
// are the nodes that must be able to use the DCS (they receive the CA and are
// granted the control plane's client port); revokeIPs are nodes that just left
// and must lose that grant.
type dcsRunConfig struct {
	jobID     string
	spec      StoredSpec
	secret    SecretInput
	clientIPs []string
	revokeIPs []string
	step      step
	timeout   time.Duration
	// See runConfig.freshDeploy. Passed to the provisioning playbook as
	// dcs_reset_stale_state.
	freshDeploy   bool
	resetHostKeys bool
}

/**
 * RunDeployStep.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg runConfig - the cfg (runConfig)
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (r *Runner) RunDeployStep(ctx context.Context, cfg runConfig) StepResult {
	return r.run(ctx, cfg)
}

/**
 * RunAddMember.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg memberRunConfig - the cfg (memberRunConfig)
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (r *Runner) RunAddMember(ctx context.Context, cfg memberRunConfig) StepResult {
	return r.runMember(ctx, cfg, r.addMemberPlaybook, "add_member")
}

/**
 * RunRemoveMember.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg memberRunConfig - the cfg (memberRunConfig)
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (r *Runner) RunRemoveMember(ctx context.Context, cfg memberRunConfig) StepResult {
	return r.runMember(ctx, cfg, r.removeMemberPlaybook, "remove_member")
}

/**
 * RunStop executes the stop playbook, which shuts Patroni down on standbys
 * first, then the primary, then stops etcd on every node. No secrets are
 * required: stopping is a pure systemd operation.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg runConfig - the cfg (runConfig); only jobID, spec and timeout are used
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (r *Runner) RunStop(ctx context.Context, cfg runConfig) StepResult {
	if strings.TrimSpace(r.stopPlaybook) == "" {
		return core.FailedStep(cfg.step.Name, fmt.Errorf("stop playbook is not configured"))
	}
	hosts := append([]string{cfg.spec.PrimaryIP}, cfg.spec.StandbyIPs...)
	if err := r.sshPolicy.EnsureKnownHosts(ctx, hosts, cfg.spec.SSHPort, false); err != nil {
		return core.FailedStep(cfg.step.Name, err)
	}
	extraVars := map[string]any{
		"deployment_job_id": cfg.jobID,
		"cluster_name":      cfg.spec.ClusterName,
		"primary_ip":        cfg.spec.PrimaryIP,
		"standby_ips":       cfg.spec.StandbyIPs,
	}
	// Carried so the stop playbook can tell the two layouts apart: with the
	// shared control plane there is no local etcd to stop, and the control
	// plane's own etcd serves every other tenant — stopping this cluster must
	// never reach it.
	mergeVars(extraVars, r.controlPlaneVars(cfg.spec, cfg.secret.DCSPassword))
	return core.AnsibleRun(ctx, core.AnsibleSpec{
		Bin:             r.ansibleBin,
		Playbook:        r.stopPlaybook,
		Inventory:       buildInventoryYAML(cfg.spec, r.sshPolicy.SSHCommonArgs()),
		ExtraVars:       extraVars,
		Verbosity:       r.ansibleVerbosity,
		StreamLogs:      r.streamLogs,
		MaxOutputChars:  r.maxOutputChars,
		Timeout:         cfg.timeout,
		StepName:        cfg.step.Name,
		WorkspacePrefix: "pgsql-cluster-stop-",
		Env:             r.sshPolicy.AnsibleEnv(),
	})
}

/**
 * RunDCSProvision creates (or converges) this cluster's tenant namespace on the
 * shared control-plane etcd and installs the control plane's CA on the nodes in
 * cfg.clientIPs. It is idempotent and runs on every deploy, start/recover and
 * add-member, which is what heals a rebuilt node whose CA file is gone.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg dcsRunConfig - the cfg (dcsRunConfig)
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (r *Runner) RunDCSProvision(ctx context.Context, cfg dcsRunConfig) StepResult {
	return r.runDCS(ctx, cfg, r.controlPlane.ProvisionPlaybook, "pgsql-dcs-provision-")
}

/**
 * RunDCSCleanup releases this cluster's tenant namespace on the shared
 * control-plane etcd: its keys, its user and its role. It runs against the
 * control plane only, so it works after the cluster's own VMs are gone.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg dcsRunConfig - the cfg (dcsRunConfig)
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (r *Runner) RunDCSCleanup(ctx context.Context, cfg dcsRunConfig) StepResult {
	return r.runDCS(ctx, cfg, r.controlPlane.CleanupPlaybook, "pgsql-dcs-cleanup-")
}

/**
 * runDCS executes one of the shared control-plane playbooks. The inventory it
 * builds is separate from the engine's own: it holds the control plane plus
 * only the nodes this operation touches, so nothing in the engine playbooks
 * (whose plays target `hosts: all`) can ever run against the control plane.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg dcsRunConfig - the cfg (dcsRunConfig)
 *   playbook string - the shared playbook to run
 *   workspacePrefix string - temp workspace prefix for this run
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (r *Runner) runDCS(ctx context.Context, cfg dcsRunConfig, playbook, workspacePrefix string) StepResult {
	if !r.controlPlane.Enabled() {
		return core.FailedStep(cfg.step.Name, fmt.Errorf("no shared control plane is configured (SHARED_CONTROL_PLANE)"))
	}
	if strings.TrimSpace(playbook) == "" {
		return core.FailedStep(cfg.step.Name, fmt.Errorf("control-plane DCS playbook is not configured"))
	}

	cpSSHPort := cfg.spec.SSHPort
	if r.controlPlane.SSHPort > 0 {
		cpSSHPort = r.controlPlane.SSHPort
	}
	// The control plane's host key is pinned like any other node's, but never
	// reset: unlike tenant VMs it is long-lived infrastructure, so a changed
	// key there is a reason to stop, not something to trust on sight.
	if err := r.sshPolicy.EnsureKnownHosts(ctx, []string{r.controlPlane.IP}, cpSSHPort, false); err != nil {
		return core.FailedStep(cfg.step.Name, err)
	}
	if len(cfg.clientIPs) > 0 {
		if err := r.sshPolicy.EnsureKnownHosts(ctx, cfg.clientIPs, cfg.spec.SSHPort, cfg.resetHostKeys); err != nil {
			return core.FailedStep(cfg.step.Name, err)
		}
	}

	target := core.ControlPlaneTarget{
		ClientIPs:         cfg.clientIPs,
		RevokeIPs:         cfg.revokeIPs,
		Scope:             cfg.spec.ClusterName,
		TenantPassword:    cfg.secret.DCSPassword,
		SSHUser:           cfg.spec.SSHUser,
		SSHPrivateKeyPath: cfg.spec.SSHPrivateKeyPath,
		SSHPort:           cfg.spec.SSHPort,
		SSHCommonArgs:     r.sshPolicy.SSHCommonArgs(),
	}
	extraVars := r.controlPlane.DCSVars(target)
	extraVars["deployment_job_id"] = cfg.jobID
	extraVars["cluster_name"] = cfg.spec.ClusterName
	// Authorises the playbook to drop an orphaned tenant prefix — see
	// runConfig.freshDeploy and the reset task in the etcd_dcs_tenant role.
	extraVars["dcs_reset_stale_state"] = cfg.freshDeploy

	return core.AnsibleRun(ctx, core.AnsibleSpec{
		Bin:             r.ansibleBin,
		Playbook:        playbook,
		Inventory:       r.controlPlane.InventoryYAML(target),
		ExtraVars:       extraVars,
		Verbosity:       r.ansibleVerbosity,
		StreamLogs:      r.streamLogs,
		MaxOutputChars:  r.maxOutputChars,
		Timeout:         cfg.timeout,
		StepName:        cfg.step.Name,
		WorkspacePrefix: workspacePrefix,
		Env:             r.sshPolicy.AnsibleEnv(),
	})
}

/**
 * run executes one tagged step of the deploy playbook.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg runConfig - the cfg (runConfig)
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (r *Runner) run(ctx context.Context, cfg runConfig) StepResult {
	hosts := append([]string{cfg.spec.PrimaryIP}, cfg.spec.StandbyIPs...)
	if err := r.sshPolicy.EnsureKnownHosts(ctx, hosts, cfg.spec.SSHPort, cfg.resetHostKeys); err != nil {
		return core.FailedStep(cfg.step.Name, err)
	}

	stepTimeout := cfg.spec.StepTimeoutSeconds
	if stepTimeout <= 0 {
		stepTimeout = 900
	}
	extraVars := map[string]any{
		"deployment_job_id":           cfg.jobID,
		"cluster_name":                cfg.spec.ClusterName,
		"primary_ip":                  cfg.spec.PrimaryIP,
		"standby_ips":                 cfg.spec.StandbyIPs,
		"postgres_superuser":          defaultPostgresSuperuser,
		"postgres_superuser_password": cfg.secret.PostgresPassword,
		"replication_user":            defaultReplicationUser,
		"replication_password":        cfg.secret.ReplicatorPassword,
		"patroni_admin_user":          cfg.spec.AdminUsername,
		"patroni_admin_password":      cfg.secret.AdminPassword,
		"postgres_exporter_password":  cfg.secret.ExporterPassword,
		"new_user":                    cfg.spec.NewUser,
		"new_user_password":           cfg.secret.NewUserPassword,
		"new_user_ssl_required":       cfg.spec.NewUserSSLRequired,
		"new_user_superuser":          cfg.spec.NewUserSuperuser,
		"new_db":                      cfg.spec.NewDB,
		"postgres_port":               cfg.spec.PostgresPort,
		"erawan_pg_major_version":     cfg.spec.PostgresVersion,
		"postgres_max_connections":    cfg.spec.ConnectionLimit,
		"postgresql_cluster_name":     defaultPostgreSQLCluster,
		"patroni_namespace":           "/db/",
		"patroni_rest_port":           8008,
		"patroni_config_path":         "/etc/patroni/patroni.yml",
		"patroni_pgpass_path":         "/etc/patroni/patroni.pgpass",
		"etcd_config_path":            "/etc/etcd/etcd.conf",
		"etcd_cluster_token":          cfg.spec.ClusterName + "-etcd-cluster-token",
		"etcd_client_port":            2379,
		"etcd_peer_port":              2380,
		"step_timeout_seconds":        stepTimeout,
		"proxy_allowed_ip":            r.proxyAllowedIP,
	}
	mergeVars(extraVars, r.controlPlaneVars(cfg.spec, cfg.secret.DCSPassword))
	return core.AnsibleRun(ctx, core.AnsibleSpec{
		Bin:             r.ansibleBin,
		Playbook:        r.deployPlaybook,
		Inventory:       buildInventoryYAML(cfg.spec, r.sshPolicy.SSHCommonArgs()),
		ExtraVars:       extraVars,
		Tags:            []string{cfg.step.Tag},
		Verbosity:       r.ansibleVerbosity,
		StreamLogs:      r.streamLogs,
		MaxOutputChars:  r.maxOutputChars,
		Timeout:         cfg.timeout,
		StepName:        cfg.step.Name,
		WorkspacePrefix: "pgsql-cluster-job-",
		Env:             r.sshPolicy.AnsibleEnv(),
	})
}

/**
 * runMember executes the add/remove-member playbook for a single node.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg memberRunConfig - the cfg (memberRunConfig)
 *   playbook string - the playbook string
 *   stepName string - the stepName string
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (r *Runner) runMember(ctx context.Context, cfg memberRunConfig, playbook, stepName string) StepResult {
	if stepName == "add_member" {
		if err := r.sshPolicy.EnsureKnownHosts(ctx, []string{cfg.memberIP}, cfg.spec.SSHPort, cfg.resetHostKeys); err != nil {
			return core.FailedStep(stepName, err)
		}
	}

	stepTimeout := cfg.spec.StepTimeoutSeconds
	if stepTimeout <= 0 {
		stepTimeout = 900
	}

	var inventory string
	sshArgs := r.sshPolicy.SSHCommonArgs()
	// effectiveStandbys reflects the expected post-operation standby list so that
	// verify_cluster's member count assertions are correct for both add and remove.
	effectiveStandbys := make([]string, len(cfg.spec.StandbyIPs))
	copy(effectiveStandbys, cfg.spec.StandbyIPs)
	if stepName == "add_member" {
		inventory = buildAddMemberInventoryYAML(cfg.spec, cfg.memberIP, sshArgs)
		effectiveStandbys = append(effectiveStandbys, cfg.memberIP)
	} else {
		// Removed node must NOT appear in pgsql_standby so the verify play
		// (hosts: pgsql_primary:pgsql_standby) does not try to SSH to a stopped node.
		// It is still present in the `all` group so Play 1 can attempt a graceful stop.
		inventory = buildRemoveMemberInventoryYAML(cfg.spec, cfg.memberIP, sshArgs)
		filtered := effectiveStandbys[:0]
		for _, ip := range effectiveStandbys {
			if ip != cfg.memberIP {
				filtered = append(filtered, ip)
			}
		}
		effectiveStandbys = filtered
	}

	extraVars := map[string]any{
		"deployment_job_id":           cfg.jobID,
		"cluster_name":                cfg.spec.ClusterName,
		"primary_ip":                  cfg.spec.PrimaryIP,
		"standby_ips":                 effectiveStandbys,
		"new_member_ip":               cfg.memberIP,
		"remove_member_ip":            cfg.memberIP,
		"force_remove":                cfg.force,
		"postgres_superuser":          defaultPostgresSuperuser,
		"postgres_superuser_password": cfg.secret.PostgresPassword,
		// Needed by the exporter play a joining node runs: without it the
		// rendered DSN carries an empty password, and the exporter comes up
		// unable to reach Postgres.
		"postgres_exporter_password": cfg.secret.ExporterPassword,
		"replication_user":           defaultReplicationUser,
		"replication_password":       cfg.secret.ReplicatorPassword,
		"patroni_admin_user":         cfg.spec.AdminUsername,
		"patroni_admin_password":     cfg.secret.AdminPassword,
		"postgres_port":              cfg.spec.PostgresPort,
		"erawan_pg_major_version":    cfg.spec.PostgresVersion,
		"postgres_max_connections":   cfg.spec.ConnectionLimit,
		"postgresql_cluster_name":    defaultPostgreSQLCluster,
		"patroni_namespace":          "/db/",
		"patroni_rest_port":          8008,
		"patroni_config_path":        "/etc/patroni/patroni.yml",
		"patroni_pgpass_path":        "/etc/patroni/patroni.pgpass",
		"etcd_config_path":           "/etc/etcd/etcd.conf",
		"etcd_cluster_token":         cfg.spec.ClusterName + "-etcd-cluster-token",
		"etcd_client_port":           2379,
		"etcd_peer_port":             2380,
		"expected_cluster_nodes":     len(effectiveStandbys) + 1,
		"step_timeout_seconds":       stepTimeout,
		"proxy_allowed_ip":           r.proxyAllowedIP,
	}
	mergeVars(extraVars, r.controlPlaneVars(cfg.spec, cfg.secret.DCSPassword))
	return core.AnsibleRun(ctx, core.AnsibleSpec{
		Bin:            r.ansibleBin,
		Playbook:       playbook,
		Inventory:      inventory,
		ExtraVars:      extraVars,
		Verbosity:      r.ansibleVerbosity,
		StreamLogs:     r.streamLogs,
		MaxOutputChars: r.maxOutputChars,
		// cfg.timeout alone only covers one of this run's several sequential
		// waits (see memberExecTimeout) -- the extra var above still carries
		// the unmodified value so Ansible's own retry math is unaffected.
		Timeout:         memberExecTimeout(stepName, cfg.timeout),
		StepName:        stepName,
		WorkspacePrefix: "pgsql-member-job-",
		Env:             r.sshPolicy.AnsibleEnv(),
	})
}

/**
 * mergeVars copies src into dst, overriding any key it already holds. Used to
 * layer the control-plane DCS settings over an engine run's base extra vars.
 *
 * Params:
 *   dst map[string]any - the extra vars being assembled
 *   src map[string]any - the overrides; nil is a no-op
 */
func mergeVars(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}

/**
 * buildAddMemberInventoryYAML.
 *
 * Params:
 *   spec StoredSpec - the spec (StoredSpec)
 *   newMemberIP string - the newMemberIP string
 *   sshCommonArgs string - the sshCommonArgs string
 *
 * Returns:
 *   string - the resulting string
 */
func buildAddMemberInventoryYAML(spec StoredSpec, newMemberIP, sshCommonArgs string) string {
	var b strings.Builder
	b.WriteString("all:\n")
	b.WriteString("  hosts:\n")
	writeHost := func(name, ip string) {
		b.WriteString("    " + name + ":\n")
		b.WriteString("      ansible_host: " + strconv.Quote(ip) + "\n")
		b.WriteString("      ansible_user: " + strconv.Quote(spec.SSHUser) + "\n")
		b.WriteString(fmt.Sprintf("      ansible_port: %d\n", spec.SSHPort))
		b.WriteString("      ansible_become: true\n")
		b.WriteString("      ansible_become_method: sudo\n")
		b.WriteString("      ansible_become_user: root\n")
		b.WriteString("      ansible_become_flags: " + strconv.Quote("-n") + "\n")
		b.WriteString("      ansible_ssh_private_key_file: " + strconv.Quote(spec.SSHPrivateKeyPath) + "\n")
		b.WriteString("      ansible_ssh_common_args: " + strconv.Quote(sshCommonArgs) + "\n")
		// Pinned so every step skips interpreter discovery (several SSH
		// round-trips per host per step). Nodes are Debian/Ubuntu, where
		// /usr/bin/python3 always exists.
		b.WriteString("      ansible_python_interpreter: /usr/bin/python3\n")
	}

	writeHost("primary", spec.PrimaryIP)
	for i, ip := range spec.StandbyIPs {
		writeHost(fmt.Sprintf("standby_%d", i+1), ip)
	}
	writeHost("new_member", newMemberIP)

	b.WriteString("  children:\n")
	b.WriteString("    pgsql_primary:\n")
	b.WriteString("      hosts:\n")
	b.WriteString("        primary: {}\n")
	b.WriteString("    pgsql_standby:\n")
	b.WriteString("      hosts:\n")
	for i := range spec.StandbyIPs {
		b.WriteString(fmt.Sprintf("        standby_%d: {}\n", i+1))
	}
	b.WriteString("    pgsql_new_member:\n")
	b.WriteString("      hosts:\n")
	b.WriteString("        new_member: {}\n")
	return b.String()
}

/**
 * buildRemoveMemberInventoryYAML builds an inventory for the remove_member playbook.
 * The removed node appears in `all` (so Play 1 can stop its services) but NOT in
 * `pgsql_standby`, so the verify play (hosts: pgsql_primary:pgsql_standby) never
 * tries to SSH to a node that has already been stopped.
 *
 * Params:
 *   spec StoredSpec - the spec (StoredSpec)
 *   removedIP string - the removedIP string
 *   sshCommonArgs string - the sshCommonArgs string
 *
 * Returns:
 *   string - the resulting string
 */
func buildRemoveMemberInventoryYAML(spec StoredSpec, removedIP, sshCommonArgs string) string {
	var b strings.Builder
	b.WriteString("all:\n")
	b.WriteString("  hosts:\n")
	writeHost := func(name, ip string) {
		b.WriteString("    " + name + ":\n")
		b.WriteString("      ansible_host: " + strconv.Quote(ip) + "\n")
		b.WriteString("      ansible_user: " + strconv.Quote(spec.SSHUser) + "\n")
		b.WriteString(fmt.Sprintf("      ansible_port: %d\n", spec.SSHPort))
		b.WriteString("      ansible_become: true\n")
		b.WriteString("      ansible_become_method: sudo\n")
		b.WriteString("      ansible_become_user: root\n")
		b.WriteString("      ansible_become_flags: " + strconv.Quote("-n") + "\n")
		b.WriteString("      ansible_ssh_private_key_file: " + strconv.Quote(spec.SSHPrivateKeyPath) + "\n")
		b.WriteString("      ansible_ssh_common_args: " + strconv.Quote(sshCommonArgs) + "\n")
		// Pinned so every step skips interpreter discovery (several SSH
		// round-trips per host per step). Nodes are Debian/Ubuntu, where
		// /usr/bin/python3 always exists.
		b.WriteString("      ansible_python_interpreter: /usr/bin/python3\n")
	}

	writeHost("primary", spec.PrimaryIP)
	standbyIdx := 1
	for _, ip := range spec.StandbyIPs {
		if ip != removedIP {
			writeHost(fmt.Sprintf("standby_%d", standbyIdx), ip)
			standbyIdx++
		}
	}
	writeHost("removed_node", removedIP)

	b.WriteString("  children:\n")
	b.WriteString("    pgsql_primary:\n")
	b.WriteString("      hosts:\n")
	b.WriteString("        primary: {}\n")
	b.WriteString("    pgsql_standby:\n")
	b.WriteString("      hosts:\n")
	for i := 1; i < standbyIdx; i++ {
		b.WriteString(fmt.Sprintf("        standby_%d: {}\n", i))
	}
	b.WriteString("    pgsql_removed:\n")
	b.WriteString("      hosts:\n")
	b.WriteString("        removed_node: {}\n")
	return b.String()
}

/**
 * buildInventoryYAML.
 *
 * Params:
 *   spec StoredSpec - the spec (StoredSpec)
 *   sshCommonArgs string - the sshCommonArgs string
 *
 * Returns:
 *   string - the resulting string
 */
func buildInventoryYAML(spec StoredSpec, sshCommonArgs string) string {
	var b strings.Builder
	b.WriteString("all:\n")
	b.WriteString("  hosts:\n")
	writeHost := func(name, ip string) {
		b.WriteString("    " + name + ":\n")
		b.WriteString("      ansible_host: " + strconv.Quote(ip) + "\n")
		b.WriteString("      ansible_user: " + strconv.Quote(spec.SSHUser) + "\n")
		b.WriteString(fmt.Sprintf("      ansible_port: %d\n", spec.SSHPort))
		b.WriteString("      ansible_become: true\n")
		b.WriteString("      ansible_become_method: sudo\n")
		b.WriteString("      ansible_become_user: root\n")
		b.WriteString("      ansible_become_flags: " + strconv.Quote("-n") + "\n")
		b.WriteString("      ansible_ssh_private_key_file: " + strconv.Quote(spec.SSHPrivateKeyPath) + "\n")
		b.WriteString("      ansible_ssh_common_args: " + strconv.Quote(sshCommonArgs) + "\n")
		// Pinned so every step skips interpreter discovery (several SSH
		// round-trips per host per step). Nodes are Debian/Ubuntu, where
		// /usr/bin/python3 always exists.
		b.WriteString("      ansible_python_interpreter: /usr/bin/python3\n")
	}

	writeHost("primary", spec.PrimaryIP)
	for i, ip := range spec.StandbyIPs {
		writeHost(fmt.Sprintf("standby_%d", i+1), ip)
	}

	b.WriteString("  children:\n")
	b.WriteString("    pgsql_primary:\n")
	b.WriteString("      hosts:\n")
	b.WriteString("        primary: {}\n")
	b.WriteString("    pgsql_standby:\n")
	b.WriteString("      hosts:\n")
	for i := range spec.StandbyIPs {
		b.WriteString(fmt.Sprintf("        standby_%d: {}\n", i+1))
	}
	return b.String()
}
