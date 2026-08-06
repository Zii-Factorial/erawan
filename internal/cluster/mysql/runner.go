package mysql

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"erawan-cluster/internal/cluster/core"
)

type Runner struct {
	ansibleBin           string
	deployPlaybook       string
	rollbackPlaybook     string
	addMemberPlaybook    string
	removeMemberPlaybook string
	stopPlaybook         string
	ansibleVerbosity     int
	streamLogs           bool
	maxOutputChars       int
	sshPolicy            core.SSHPolicy
	proxyAllowedIP       string
}

/**
 * NewRunner.
 *
 * Params:
 *   ansibleBin string - the ansibleBin string
 *   deployPlaybook string - the deployPlaybook string
 *   rollbackPlaybook string - the rollbackPlaybook string
 *
 * Returns:
 *   *Runner - the resulting *Runner
 */
func NewRunner(ansibleBin, deployPlaybook, rollbackPlaybook string) *Runner {
	if strings.TrimSpace(ansibleBin) == "" {
		ansibleBin = "ansible-playbook"
	}
	return &Runner{
		ansibleBin:       ansibleBin,
		deployPlaybook:   deployPlaybook,
		rollbackPlaybook: rollbackPlaybook,
		maxOutputChars:   8000,
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
 * node-to-node access (GR management, replication) is scoped separately by peer
 * IP, not by this value.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ip string - the allowed proxy IP
 */
func (r *Runner) SetProxyAllowedIP(ip string) { r.proxyAllowedIP = ip }

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
	jobID         string
	spec          StoredSpec
	secret        SecretInput
	step          step
	timeout       time.Duration
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
	return r.run(ctx, cfg, r.deployPlaybook)
}

/**
 * RunRollback.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   jobID string - the jobID string
 *   spec StoredSpec - the spec (StoredSpec)
 *   secret SecretInput - the secret (SecretInput)
 *   timeout time.Duration - the timeout (time.Duration)
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (r *Runner) RunRollback(ctx context.Context, jobID string, spec StoredSpec, secret SecretInput, timeout time.Duration) StepResult {
	cfg := runConfig{
		jobID:   jobID,
		spec:    spec,
		secret:  secret,
		step:    step{Name: "rollback", Tag: "rollback"},
		timeout: timeout,
	}
	return r.run(ctx, cfg, r.rollbackPlaybook)
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
 * RunStop executes the stop playbook, which shuts MySQL down on secondaries
 * first, then the primary. No secrets are required: stopping is a pure
 * systemd operation.
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
		WorkspacePrefix: "mysql-cluster-stop-",
		Env:             r.sshPolicy.AnsibleEnv(),
	})
}

/**
 * run executes one tagged step of a deploy/rollback playbook.
 *
 * Receiver:
 *   r *Runner - pointer receiver; the method may mutate this Runner instance
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   cfg runConfig - the cfg (runConfig)
 *   playbook string - the playbook string
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func (r *Runner) run(ctx context.Context, cfg runConfig, playbook string) StepResult {
	hosts := append([]string{cfg.spec.PrimaryIP}, cfg.spec.StandbyIPs...)
	if err := r.sshPolicy.EnsureKnownHosts(ctx, hosts, cfg.spec.SSHPort, cfg.resetHostKeys); err != nil {
		return core.FailedStep(cfg.step.Name, err)
	}

	stepTimeout := cfg.spec.StepTimeoutSeconds
	if stepTimeout <= 0 {
		stepTimeout = 900
	}
	extraVars := map[string]any{
		"cluster_name":               cfg.spec.ClusterName,
		"cluster_admin_username":     cfg.spec.AdminUsername,
		"cluster_admin_password":     cfg.secret.AdminPassword,
		"primary_ip":                 cfg.spec.PrimaryIP,
		"standby_ips":                cfg.spec.StandbyIPs,
		"new_user":                   cfg.spec.NewUser,
		"new_user_password":          cfg.secret.NewUserPassword,
		"new_user_ssl_required":      cfg.spec.NewUserSSLRequired,
		"new_user_superuser":         cfg.spec.NewUserSuperuser,
		"new_db":                     cfg.spec.NewDB,
		"mysql_port":                 cfg.spec.MySQLPort,
		"erawan_mysql_major_version": cfg.spec.MySQLVersion,
		"mysql_max_connections":      cfg.spec.ConnectionLimit,
		"assume_prepared":            cfg.spec.AssumePrepared,
		"step_timeout_seconds":       stepTimeout,
		"proxy_allowed_ip":           r.proxyAllowedIP,
	}
	return core.AnsibleRun(ctx, core.AnsibleSpec{
		Bin:             r.ansibleBin,
		Playbook:        playbook,
		Inventory:       buildInventoryYAML(cfg.spec, r.sshPolicy.SSHCommonArgs()),
		ExtraVars:       extraVars,
		Tags:            []string{cfg.step.Tag},
		Verbosity:       r.ansibleVerbosity,
		StreamLogs:      r.streamLogs,
		MaxOutputChars:  r.maxOutputChars,
		Timeout:         cfg.timeout,
		StepName:        cfg.step.Name,
		WorkspacePrefix: "mysql-cluster-job-",
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
	// verify_cluster's EXPECTED_CLUSTER_NODES count is correct for both add and remove.
	effectiveStandbys := make([]string, len(cfg.spec.StandbyIPs))
	copy(effectiveStandbys, cfg.spec.StandbyIPs)
	if stepName == "add_member" {
		inventory = buildAddMemberInventoryYAML(cfg.spec, cfg.memberIP, sshArgs)
		effectiveStandbys = append(effectiveStandbys, cfg.memberIP)
	} else {
		inventory = buildInventoryYAML(cfg.spec, sshArgs)
		filtered := effectiveStandbys[:0]
		for _, ip := range effectiveStandbys {
			if ip != cfg.memberIP {
				filtered = append(filtered, ip)
			}
		}
		effectiveStandbys = filtered
	}

	extraVars := map[string]any{
		"cluster_name":               cfg.spec.ClusterName,
		"cluster_admin_username":     cfg.spec.AdminUsername,
		"cluster_admin_password":     cfg.secret.AdminPassword,
		"primary_ip":                 cfg.spec.PrimaryIP,
		"standby_ips":                effectiveStandbys,
		"new_member_ip":              cfg.memberIP,
		"remove_member_ip":           cfg.memberIP,
		"force_remove":               cfg.force,
		"mysql_port":                 cfg.spec.MySQLPort,
		"erawan_mysql_major_version": cfg.spec.MySQLVersion,
		"mysql_max_connections":      cfg.spec.ConnectionLimit,
		"assume_prepared":            cfg.spec.AssumePrepared,
		"step_timeout_seconds":       stepTimeout,
		"expected_cluster_nodes":     len(effectiveStandbys) + 1,
		"proxy_allowed_ip":           r.proxyAllowedIP,
	}
	return core.AnsibleRun(ctx, core.AnsibleSpec{
		Bin:             r.ansibleBin,
		Playbook:        playbook,
		Inventory:       inventory,
		ExtraVars:       extraVars,
		Verbosity:       r.ansibleVerbosity,
		StreamLogs:      r.streamLogs,
		MaxOutputChars:  r.maxOutputChars,
		Timeout:         cfg.timeout,
		StepName:        stepName,
		WorkspacePrefix: "mysql-member-job-",
		Env:             r.sshPolicy.AnsibleEnv(),
	})
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
	b.WriteString("    mysql_primary:\n")
	b.WriteString("      hosts:\n")
	b.WriteString("        primary: {}\n")
	b.WriteString("    mysql_standby:\n")
	b.WriteString("      hosts:\n")
	for i := range spec.StandbyIPs {
		b.WriteString(fmt.Sprintf("        standby_%d: {}\n", i+1))
	}
	b.WriteString("    mysql_new_member:\n")
	b.WriteString("      hosts:\n")
	b.WriteString("        new_member: {}\n")
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
	b.WriteString("    mysql_primary:\n")
	b.WriteString("      hosts:\n")
	b.WriteString("        primary: {}\n")
	b.WriteString("    mysql_standby:\n")
	b.WriteString("      hosts:\n")
	for i := range spec.StandbyIPs {
		b.WriteString(fmt.Sprintf("        standby_%d: {}\n", i+1))
	}
	return b.String()
}
