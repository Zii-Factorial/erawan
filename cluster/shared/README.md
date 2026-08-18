# Shared control-plane playbooks

Engine-agnostic Ansible for the **shared control plane** — the host that carries
a distributed configuration store (etcd) on behalf of database engines that have
no quorum mechanism of their own.

## Why this exists

PostgreSQL is the motivating case. Patroni needs a DCS to elect a leader, and
without a shared one every tenant has to run its own etcd quorum **on its own
data nodes**. That has two consequences a hosting platform cannot design around:

- a healthy cluster needs an odd node count (a 2-node cluster has no quorum), and
- the tenant's HA is only as good as the tenant's own two or three VMs — losing
  one etcd member is enough to freeze failover.

MySQL does not have this problem: Group Replication carries its own quorum.

Pointing every tenant at one already-quorate control plane moves the quorum
requirement off the data plane entirely. A PostgreSQL cluster of **any** size —
one node included — then works, and the DB nodes run no etcd at all.

## What a tenant gets

Each cluster (tenant) is issued exactly one etcd role, one user and one key
prefix on the shared control plane:

```
role  patroni-<cluster>-role   readwrite on /db/patroni/<cluster>/
user  patroni-<cluster>-user   granted that role and nothing else
keys  /db/patroni/<cluster>/…  Patroni's leader, config, members, …
```

etcd RBAC is prefix-based, which is what keeps tenants off each other's keys on
infrastructure they all share. The DB nodes hold only the control plane's CA and
their own username/password — never a client certificate, never the root
credential, and no reach outside their own prefix.

## Playbooks

| Playbook | Runs against | Does |
|----------|--------------|------|
| `control_plane_dcs_provision.yml` | control plane + the cluster's nodes | Creates/converges the tenant's role, user and grant; installs the control plane's CA on each node and verifies the node can reach and trust it; opens the control plane's client port to those nodes |
| `control_plane_dcs_cleanup.yml` | control plane only | Deletes the tenant's keys, user and role, and revokes its nodes' access |

Both are idempotent. Provision runs on every deploy, start/recover and
add-member — that is what heals a node rebuilt by a scale operation (its CA file
is gone and the control plane has no firewall grant for its new address).

Cleanup runs against the control plane alone, so it works after the cluster's own
VMs are destroyed. Call it when a cluster is decommissioned: the tenant's objects
are named after the cluster, so leaving them behind accumulates state on shared
infrastructure and a later cluster of the same name would inherit them.

## Roles

- `etcd_dcs_tenant` — all control-plane-side work. `dcs_action` selects
  `provision` or `cleanup`.
- `etcd_dcs_client` — the node-side half: installs the CA and proves the node can
  reach the control plane and validate its certificate **against the IP it
  dials**. That last check is deliberate: a server certificate without that IP in
  its SANs produces a cluster that starts and then fails every DCS call with a
  TLS error buried in the engine's journal.

## What is assumed, and what is not

The control plane itself — its etcd, TLS material, root user and `auth enable` —
is expected to exist already. Erawan consumes it and never provisions it, which
is why the roles fail early and by name when `/etc/etcd/ssl/*.pem` is missing or
the root credential is wrong.

## Wiring a new engine

Nothing here mentions PostgreSQL. An engine adopts the control plane by:

1. Handing its Ansible runner a `core.ControlPlane` (see
   `internal/cluster/core/controlplane.go`), which builds the inventory and the
   `dcs_*` extra vars these playbooks consume.
2. Running `control_plane_dcs_provision.yml` as a step of its deploy and
   start/recover flows, and before each add-member.
3. Overriding `dcs_client_ca_path` / `dcs_client_ca_owner` if its service account
   is not `postgres`, and rendering the endpoint, CA path, username and password
   into its own config template.
4. Gating its local-DCS tasks on the mode flag so nothing installs, starts or
   verifies a per-node DCS that no longer exists — the PostgreSQL playbooks use
   `erawan_dcs_control_plane` for this.

See `cluster/pgsql/README.md` for how PostgreSQL does each of those.
