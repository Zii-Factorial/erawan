package core

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ControlPlane describes the shared control-plane host that carries the
// distributed configuration store (etcd) on behalf of engines that have no
// quorum mechanism of their own.
//
// PostgreSQL is the motivating case: Patroni needs a DCS, so without a shared
// control plane every tenant has to run its own etcd quorum on its own data
// nodes — which makes a healthy cluster an odd-node-count affair and puts the
// tenant's HA at the mercy of its own two or three VMs. Pointing every tenant
// at one already-quorate control plane moves that requirement off the data
// plane entirely.
//
// Nothing here is PostgreSQL-specific. An engine wires it up by handing its
// runner a ControlPlane and running the two shared playbooks
// (cluster/shared/playbooks/control_plane_dcs_{provision,cleanup}.yml); the
// engine only has to map DCSVars/tenant names into whatever its own config
// template expects.
//
// The control plane itself (TLS material, root user, auth enable) is assumed to
// exist already — erawan consumes it and never provisions it. Each tenant gets
// exactly one etcd role, one user and one key prefix on it:
//
//	role  <prefix>-<scope>-role  readwrite on <namespace><scope>/
//	user  <prefix>-<scope>-user  granted that role and nothing else
type ControlPlane struct {
	// IP is the control plane's private address (SHARED_CONTROL_PLANE). Empty
	// disables the whole feature: engines then keep their classic per-node DCS.
	IP string
	// EtcdClientPort is the port both the DB nodes and etcdctl dial.
	EtcdClientPort int

	// CACertPath, CertPath and KeyPath are paths ON the control-plane host,
	// used by etcdctl over SSH. The server key never leaves the control plane;
	// only the CA does, plus per-tenant client certificates minted from it.
	CACertPath string
	CertPath   string
	KeyPath    string

	// ClientCert controls whether each tenant is issued its own client
	// certificate. Required by a control plane running with client-cert-auth,
	// which refuses the TLS handshake to a client presenting none. CAKeyPath
	// is the CA private key that signs them, and ClientCertDir is where the
	// minted pairs are kept on the control plane so they can be inspected,
	// rotated or revoked.
	ClientCert     bool
	CAKeyPath      string
	ClientCertDir  string
	ClientCertDays int

	// RootUser/RootPassword own RBAC on the shared etcd. Used only by the
	// control-plane plays; a DB node never sees them.
	RootUser     string
	RootPassword string

	// Namespace is the key prefix every tenant hangs off, slash-delimited
	// (e.g. "/db/patroni/"). Tenant keys live under Namespace + scope + "/".
	Namespace string
	// TenantNamePrefix names the per-tenant etcd role and user.
	TenantNamePrefix string

	// NodeCAPath is where the control plane's CA is installed on each DB node,
	// NodeCertPath/NodeKeyPath where that node's client certificate goes, and
	// NodeCAOwner is the OS user that must be able to read them (the engine's
	// service account — postgres for Patroni).
	NodeCAPath   string
	NodeCertPath string
	NodeKeyPath  string
	NodeCAOwner  string

	// SSH overrides for reaching the control plane. Empty fields fall back to
	// the cluster's own SSH settings: the control plane is built from the same
	// node template, so the same key normally works.
	SSHUser           string
	SSHPrivateKeyPath string
	SSHPort           int

	ProvisionPlaybook string
	CleanupPlaybook   string
}

// ControlPlaneTarget carries the per-run bits the shared playbooks need: which
// cluster nodes are involved and how to reach them over SSH.
type ControlPlaneTarget struct {
	// ClientIPs are the cluster nodes that must be able to use the DCS. They
	// receive the CA and are granted the control plane's client port.
	ClientIPs []string
	// RevokeIPs are nodes that just left the cluster; their grant on the
	// control plane is dropped.
	RevokeIPs []string
	// Scope identifies the tenant — the cluster name.
	Scope string
	// TenantPassword is the per-tenant etcd password held in the job's secret.
	TenantPassword string

	SSHUser           string
	SSHPrivateKeyPath string
	SSHPort           int
	SSHCommonArgs     string
}

/**
 * Enabled reports whether a shared control plane is configured. When false the
 * engine keeps its classic per-node DCS and every control-plane step is skipped.
 *
 * Receiver:
 *   c ControlPlane - value receiver; the method operates on a copy
 *
 * Returns:
 *   bool - true when a control-plane address is set
 */
func (c ControlPlane) Enabled() bool { return strings.TrimSpace(c.IP) != "" }

/**
 * Validate checks that an enabled control plane is fully configured. It is
 * called at start-up so a half-configured control plane fails the process
 * immediately instead of producing clusters that cannot reach their DCS.
 *
 * Receiver:
 *   c ControlPlane - value receiver; the method operates on a copy
 *
 * Returns:
 *   error - non-nil when a required field is missing or malformed
 */
func (c ControlPlane) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if net.ParseIP(strings.TrimSpace(c.IP)) == nil {
		return fmt.Errorf("SHARED_CONTROL_PLANE must be a valid IP address, got %q", c.IP)
	}
	if strings.TrimSpace(c.RootPassword) == "" {
		return fmt.Errorf("CONTROL_PLANE_ETCD_ROOT_PASSWORD is required when SHARED_CONTROL_PLANE is set")
	}
	if c.EtcdClientPort < 1 || c.EtcdClientPort > 65535 {
		return fmt.Errorf("CONTROL_PLANE_ETCD_CLIENT_PORT must be between 1 and 65535, got %d", c.EtcdClientPort)
	}
	if !strings.HasPrefix(c.Namespace, "/") || !strings.HasSuffix(c.Namespace, "/") {
		return fmt.Errorf("CONTROL_PLANE_DCS_NAMESPACE must start and end with %q, got %q", "/", c.Namespace)
	}
	if strings.TrimSpace(c.ProvisionPlaybook) == "" || strings.TrimSpace(c.CleanupPlaybook) == "" {
		return fmt.Errorf("control-plane DCS provision and cleanup playbooks must both be configured")
	}
	return nil
}

/**
 * TenantUser returns the etcd username issued to one tenant.
 *
 * Receiver:
 *   c ControlPlane - value receiver; the method operates on a copy
 *
 * Params:
 *   scope string - the tenant scope (cluster name)
 *
 * Returns:
 *   string - the resulting username
 */
func (c ControlPlane) TenantUser(scope string) string {
	return c.TenantNamePrefix + "-" + scope + "-user"
}

/**
 * TenantRole returns the etcd role issued to one tenant.
 *
 * Receiver:
 *   c ControlPlane - value receiver; the method operates on a copy
 *
 * Params:
 *   scope string - the tenant scope (cluster name)
 *
 * Returns:
 *   string - the resulting role name
 */
func (c ControlPlane) TenantRole(scope string) string {
	return c.TenantNamePrefix + "-" + scope + "-role"
}

/**
 * KeyPrefix returns the etcd key prefix a tenant owns. It is both what the
 * tenant's role is granted and what cleanup deletes, so the two can never
 * disagree.
 *
 * Receiver:
 *   c ControlPlane - value receiver; the method operates on a copy
 *
 * Params:
 *   scope string - the tenant scope (cluster name)
 *
 * Returns:
 *   string - the resulting key prefix
 */
func (c ControlPlane) KeyPrefix(scope string) string {
	return c.Namespace + scope + "/"
}

/**
 * DCSVars builds the extra vars for the shared control-plane playbooks. The
 * names are engine-neutral (dcs_*) because the playbooks are shared by every
 * engine that needs an external DCS.
 *
 * Receiver:
 *   c ControlPlane - value receiver; the method operates on a copy
 *
 * Params:
 *   t ControlPlaneTarget - the tenant scope, credential and node lists
 *
 * Returns:
 *   map[string]any - the resulting extra vars
 */
func (c ControlPlane) DCSVars(t ControlPlaneTarget) map[string]any {
	return map[string]any{
		"dcs_endpoint_ip":     c.IP,
		"dcs_client_port":     c.EtcdClientPort,
		"dcs_ca_file":         c.CACertPath,
		"dcs_cert_file":       c.CertPath,
		"dcs_key_file":        c.KeyPath,
		"dcs_root_user":       c.RootUser,
		"dcs_root_password":   c.RootPassword,
		"dcs_namespace":       c.Namespace,
		"dcs_scope":           t.Scope,
		"dcs_tenant_user":     c.TenantUser(t.Scope),
		"dcs_tenant_role":     c.TenantRole(t.Scope),
		"dcs_tenant_password": t.TenantPassword,
		"dcs_client_ips":      orEmpty(t.ClientIPs),
		"dcs_revoke_ips":      orEmpty(t.RevokeIPs),
		"dcs_client_ca_path":  c.NodeCAPath,
		"dcs_client_ca_owner": c.NodeCAOwner,
		"dcs_client_ca_group": c.NodeCAOwner,

		"dcs_client_cert_enabled": c.ClientCert,
		"dcs_ca_key_file":         c.CAKeyPath,
		"dcs_client_cert_dir":     c.ClientCertDir,
		"dcs_client_cert_days":    c.ClientCertDays,
		"dcs_client_cert_path":    c.NodeCertPath,
		"dcs_client_key_path":     c.NodeKeyPath,
	}
}

/**
 * orEmpty returns a non-nil slice. A nil slice marshals to JSON null, which
 * Ansible treats as a DEFINED variable holding None — so `| default([])` in a
 * playbook never fires for it and any list filter downstream raises. Every
 * control-plane run leaves one of the two node lists empty (a cleanup has no
 * clients, a deploy has nothing to revoke), so this is the normal case, not an
 * edge one.
 *
 * Params:
 *   in []string - the slice to normalize
 *
 * Returns:
 *   []string - in, or an empty non-nil slice when in is nil
 */
func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

/**
 * InventoryYAML builds the inventory for the shared control-plane playbooks:
 * the control plane in group control_plane, plus the cluster nodes that need
 * the DCS in group dcs_clients (empty for cleanup, which never touches them).
 *
 * The cluster's own engine inventories deliberately do NOT contain the control
 * plane: their plays run against `hosts: all`, and the control plane must never
 * be swept into a play meant for database nodes.
 *
 * Receiver:
 *   c ControlPlane - value receiver; the method operates on a copy
 *
 * Params:
 *   t ControlPlaneTarget - node list and SSH settings for this run
 *
 * Returns:
 *   string - the resulting inventory YAML
 */
func (c ControlPlane) InventoryYAML(t ControlPlaneTarget) string {
	sshUser := t.SSHUser
	if strings.TrimSpace(c.SSHUser) != "" {
		sshUser = c.SSHUser
	}
	sshKey := t.SSHPrivateKeyPath
	if strings.TrimSpace(c.SSHPrivateKeyPath) != "" {
		sshKey = c.SSHPrivateKeyPath
	}
	sshPort := t.SSHPort
	if c.SSHPort > 0 {
		sshPort = c.SSHPort
	}

	var b strings.Builder
	b.WriteString("all:\n")
	b.WriteString("  hosts:\n")
	writeHost := func(name, ip, user, key string, port int) {
		b.WriteString("    " + name + ":\n")
		b.WriteString("      ansible_host: " + strconv.Quote(ip) + "\n")
		b.WriteString("      ansible_user: " + strconv.Quote(user) + "\n")
		b.WriteString(fmt.Sprintf("      ansible_port: %d\n", port))
		b.WriteString("      ansible_become: true\n")
		b.WriteString("      ansible_become_method: sudo\n")
		b.WriteString("      ansible_become_user: root\n")
		b.WriteString("      ansible_become_flags: " + strconv.Quote("-n") + "\n")
		b.WriteString("      ansible_ssh_private_key_file: " + strconv.Quote(key) + "\n")
		b.WriteString("      ansible_ssh_common_args: " + strconv.Quote(t.SSHCommonArgs) + "\n")
		// Pinned for the same reason as the engine inventories: it skips
		// interpreter discovery, which costs several SSH round-trips per host.
		b.WriteString("      ansible_python_interpreter: /usr/bin/python3\n")
	}

	// The host is named cp_etcd, not control_plane: a host and a group may not
	// share a name in an Ansible inventory.
	writeHost("cp_etcd", c.IP, sshUser, sshKey, sshPort)
	for i, ip := range t.ClientIPs {
		writeHost(fmt.Sprintf("dcs_client_%d", i+1), ip, t.SSHUser, t.SSHPrivateKeyPath, t.SSHPort)
	}

	b.WriteString("  children:\n")
	b.WriteString("    control_plane:\n")
	b.WriteString("      hosts:\n")
	b.WriteString("        cp_etcd: {}\n")
	b.WriteString("    dcs_clients:\n")
	b.WriteString("      hosts:\n")
	for i := range t.ClientIPs {
		b.WriteString(fmt.Sprintf("        dcs_client_%d: {}\n", i+1))
	}
	return b.String()
}
