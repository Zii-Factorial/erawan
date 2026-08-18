package core_test

import (
	"encoding/json"
	"strings"
	"testing"

	"erawan-cluster/internal/cluster/core"
)

// enabledControlPlane mirrors what loadControlPlaneConfig produces for a
// configured shared control plane.
func enabledControlPlane() core.ControlPlane {
	return core.ControlPlane{
		IP:                "10.10.3.66",
		EtcdClientPort:    2379,
		CACertPath:        "/etc/etcd/ssl/ca.pem",
		CertPath:          "/etc/etcd/ssl/cp-etcd-01.pem",
		KeyPath:           "/etc/etcd/ssl/cp-etcd-01-key.pem",
		RootUser:          "root",
		RootPassword:      "root-secret",
		Namespace:         "/db/patroni/",
		TenantNamePrefix:  "patroni",
		NodeCAPath:        "/etc/patroni/etcd-ca.pem",
		NodeCAOwner:       "postgres",
		ProvisionPlaybook: "/opt/cluster/shared/playbooks/control_plane_dcs_provision.yml",
		CleanupPlaybook:   "/opt/cluster/shared/playbooks/control_plane_dcs_cleanup.yml",
	}
}

func TestControlPlaneDisabledByDefault(t *testing.T) {
	var cp core.ControlPlane
	if cp.Enabled() {
		t.Fatal("zero-value control plane must be disabled")
	}
	// A disabled control plane must never fail start-up: it is the default for
	// every deployment that has not opted in.
	if err := cp.Validate(); err != nil {
		t.Fatalf("disabled control plane must validate, got %v", err)
	}
}

func TestControlPlaneValidateRejectsIncompleteConfig(t *testing.T) {
	cases := map[string]func(*core.ControlPlane){
		"missing root password": func(c *core.ControlPlane) { c.RootPassword = "" },
		"non-IP address":        func(c *core.ControlPlane) { c.IP = "control-plane.internal" },
		"out-of-range port":     func(c *core.ControlPlane) { c.EtcdClientPort = 0 },
		"unslashed namespace":   func(c *core.ControlPlane) { c.Namespace = "db/patroni" },
		"no cleanup playbook":   func(c *core.ControlPlane) { c.CleanupPlaybook = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cp := enabledControlPlane()
			mutate(&cp)
			if err := cp.Validate(); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}

	if err := enabledControlPlane().Validate(); err != nil {
		t.Fatalf("fully configured control plane must validate, got %v", err)
	}
}

func TestControlPlaneTenantNaming(t *testing.T) {
	cp := enabledControlPlane()
	scope := "tenant-fungicloud-001"

	if got, want := cp.TenantUser(scope), "patroni-tenant-fungicloud-001-user"; got != want {
		t.Fatalf("tenant user = %q, want %q", got, want)
	}
	if got, want := cp.TenantRole(scope), "patroni-tenant-fungicloud-001-role"; got != want {
		t.Fatalf("tenant role = %q, want %q", got, want)
	}
	// The prefix is what the tenant's etcd role is granted and what cleanup
	// deletes, and it must stay inside the shared namespace: a prefix that lost
	// its scope would match every other tenant's keys.
	if got, want := cp.KeyPrefix(scope), "/db/patroni/tenant-fungicloud-001/"; got != want {
		t.Fatalf("key prefix = %q, want %q", got, want)
	}
}

func TestControlPlaneDCSVarsCarryTenantAndCredentials(t *testing.T) {
	cp := enabledControlPlane()
	vars := cp.DCSVars(core.ControlPlaneTarget{
		Scope:          "tenant-a1",
		TenantPassword: "tenant-secret",
		ClientIPs:      []string{"10.10.1.10", "10.10.1.11"},
		RevokeIPs:      []string{"10.10.1.12"},
	})

	want := map[string]any{
		"dcs_endpoint_ip":     "10.10.3.66",
		"dcs_client_port":     2379,
		"dcs_root_password":   "root-secret",
		"dcs_scope":           "tenant-a1",
		"dcs_tenant_user":     "patroni-tenant-a1-user",
		"dcs_tenant_role":     "patroni-tenant-a1-role",
		"dcs_tenant_password": "tenant-secret",
		"dcs_client_ca_path":  "/etc/patroni/etcd-ca.pem",
	}
	for k, v := range want {
		if vars[k] != v {
			t.Errorf("%s = %v, want %v", k, vars[k], v)
		}
	}
	if got := vars["dcs_client_ips"].([]string); len(got) != 2 {
		t.Errorf("dcs_client_ips = %v, want two nodes", got)
	}
	if got := vars["dcs_revoke_ips"].([]string); len(got) != 1 {
		t.Errorf("dcs_revoke_ips = %v, want one node", got)
	}
}

func TestControlPlaneInventoryGroupsHostsAndSSH(t *testing.T) {
	cp := enabledControlPlane()
	inv := cp.InventoryYAML(core.ControlPlaneTarget{
		Scope:             "tenant-a1",
		ClientIPs:         []string{"10.10.1.10", "10.10.1.11"},
		SSHUser:           "clusterops",
		SSHPrivateKeyPath: "/var/lib/erawan-cluster/keys/clusterops_rsa",
		SSHPort:           22,
		SSHCommonArgs:     "-o StrictHostKeyChecking=no",
	})

	for _, want := range []string{
		"    cp_etcd:",
		"      ansible_host: \"10.10.3.66\"",
		"    dcs_client_1:",
		"    dcs_client_2:",
		"    control_plane:",
		"    dcs_clients:",
		"        cp_etcd: {}",
		"        dcs_client_2: {}",
	} {
		if !strings.Contains(inv, want) {
			t.Errorf("inventory missing %q:\n%s", want, inv)
		}
	}
	// The control plane must never be reachable as a database node: a play that
	// targets `hosts: all` in an engine playbook would otherwise sweep it in.
	if strings.Contains(inv, "pgsql_primary") || strings.Contains(inv, "pgsql_standby") {
		t.Errorf("control-plane inventory must not carry engine groups:\n%s", inv)
	}
}

func TestControlPlaneInventoryHonoursSSHOverrides(t *testing.T) {
	cp := enabledControlPlane()
	cp.SSHUser = "cpadmin"
	cp.SSHPrivateKeyPath = "/var/lib/erawan-cluster/keys/cp_rsa"
	cp.SSHPort = 2222

	inv := cp.InventoryYAML(core.ControlPlaneTarget{
		ClientIPs:         []string{"10.10.1.10"},
		SSHUser:           "clusterops",
		SSHPrivateKeyPath: "/var/lib/erawan-cluster/keys/clusterops_rsa",
		SSHPort:           22,
	})

	cpHost, clientHost, found := strings.Cut(inv, "    dcs_client_1:")
	if !found {
		t.Fatalf("inventory has no client host:\n%s", inv)
	}
	// Overrides apply to the control plane only — the cluster's own nodes keep
	// the credentials the job was deployed with.
	if !strings.Contains(cpHost, "cpadmin") || !strings.Contains(cpHost, "ansible_port: 2222") {
		t.Errorf("control-plane host ignored SSH overrides:\n%s", cpHost)
	}
	if !strings.Contains(clientHost, "clusterops") || !strings.Contains(clientHost, "ansible_port: 22") {
		t.Errorf("client host must keep the cluster's own SSH settings:\n%s", clientHost)
	}
}

func TestControlPlaneDCSVarsNeverEmitNullLists(t *testing.T) {
	cp := enabledControlPlane()

	// A cleanup run carries no client nodes and a deploy carries nothing to
	// revoke, so one of the two lists is always nil on the Go side. Marshalled
	// as JSON null it reaches Ansible as a DEFINED None, which `| default([])`
	// does not catch and every list filter downstream raises on.
	for name, target := range map[string]core.ControlPlaneTarget{
		"cleanup (no clients)": {Scope: "t1", RevokeIPs: []string{"10.10.1.10"}},
		"deploy (no revokes)":  {Scope: "t1", ClientIPs: []string{"10.10.1.10"}},
		"neither":              {Scope: "t1"},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(cp.DCSVars(target))
			if err != nil {
				t.Fatalf("marshal vars: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("unmarshal vars: %v", err)
			}
			for _, key := range []string{"dcs_client_ips", "dcs_revoke_ips"} {
				if decoded[key] == nil {
					t.Errorf("%s marshalled to null; Ansible needs an empty list", key)
				}
			}
		})
	}
}
