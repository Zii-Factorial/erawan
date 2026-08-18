# PostgreSQL Cluster Ansible

This folder contains the PostgreSQL HA cluster automation used by the API.

Supported topologies:

- 1 PostgreSQL primary node only
- 1 PostgreSQL primary node plus 1 or more standby nodes

Implemented workflow:

- PostgreSQL + `patroni` + `etcd` preflight checks
- Shared `etcd` cluster configuration on all PostgreSQL nodes
- Patroni leader bootstrap on the requested primary node
- Patroni replica bootstrap on standby nodes when `standby_ips` is provided
- Retry-safe bootstrap resets data only once per deployment job
- Cluster verification through systemd state, Patroni REST API, and replication checks
- Optional application database/user bootstrap
- Add member: etcd learner registration → join → promote to voter → Patroni
  standby bootstrap (one node at a time); stale etcd registrations from
  destroyed nodes are cleaned up first, with guarded `force-new-cluster`
  recovery when they have cost the primary its quorum
- Remove member: graceful Patroni + etcd removal of a standby
- Stop: data-preserving ordered shutdown (standbys → primary → etcd)
- Start / recover: restart a stopped or outage-hit cluster from existing data
  (`cluster_bootstrap` + `verify_cluster`; data directories untouched)

Architecture overview:

```text
      API / Ansible
           |
           v
   +------------------+
   | Patroni services |
   | on all PG nodes  |
   +------------------+
           |
           v
   +------------------+
   | PostgreSQL       |
   | leader + optional|
   | replicas         |
   +------------------+
           ^
           |
   +------------------+
   | etcd cluster     |
   | shared DCS state |
   +------------------+
```

Entry points:

- `cluster/pgsql/playbooks/deploy.yml`
- `cluster/pgsql/playbooks/add_member.yml`
- `cluster/pgsql/playbooks/remove_member.yml`
- `cluster/pgsql/playbooks/stop.yml`

## DCS layout: per-node etcd or the shared control plane

PostgreSQL has no quorum mechanism of its own, so Patroni needs a DCS. These
playbooks support two layouts, chosen per cluster at deploy time and recorded on
the job — flipping the environment variable never re-points a live cluster.

**Classic (default).** One etcd per PostgreSQL node, as drawn above. A healthy
cluster needs an odd node count, and the tenant's HA depends on the tenant's own
VMs. Everything about etcd membership — learner registration, promotion, stale
voter cleanup, `force-new-cluster` recovery — exists to keep that quorum alive.

**Shared control plane.** Set `SHARED_CONTROL_PLANE` to the control plane's
private IP and new clusters run **no etcd at all**. Patroni uses the control
plane's etcd over TLS with a per-tenant user, so any node count works — one node
included — and the quorum lives on shared infrastructure instead of the tenant's:

```text
   +------------------+          +---------------------------+
   | Patroni services |  https   | shared control-plane etcd |
   | on all PG nodes  | -------> | /db/patroni/<cluster>/    |
   +------------------+   TLS +  | tenant user + role        |
                          auth   +---------------------------+
```

Every etcd task in these playbooks is gated on `erawan_dcs_control_plane`, so in
this layout nothing installs, starts, verifies or stops a local etcd, and no
firewall rule is opened for one. The tenant's namespace on the control plane is
created by `cluster/shared/playbooks/control_plane_dcs_provision.yml`, which the
Go runner runs as the `control_plane_dcs` step of every deploy, start/recover and
add-member; `DELETE /cluster/pgsql/dcs` releases it when the cluster is
decommissioned. See `cluster/shared/README.md`.

Node-side settings written into `/etc/patroni/patroni.yml` (`etcd3` section, v3
API — the control plane runs RBAC, which the v2 endpoints do not carry):

| Var | Meaning |
|-----|---------|
| `control_plane_ip` | Control-plane address; empty selects the classic layout |
| `control_plane_etcd_client_port` | Client port to dial (default 2379) |
| `control_plane_etcd_user` / `_password` | The cluster's per-tenant credential |
| `patroni_etcd_ca_path` | Control-plane CA installed on the node |
| `patroni_namespace` | `/db/patroni/` — keys land in `<namespace><cluster>/` |

Patroni's `etcd3` client reaches the control plane over **HTTP** (`POST /v3/...`),
which etcd serves only with `enable-grpc-gateway: true` — and that is *not* the
default when etcd is started from a config file. The control plane's other hard
requirements (tenant certificates with no CommonName, a server certificate
carrying `clientAuth`, `auth enable`) are listed in `cluster/shared/README.md`,
with a reference config and symptom index in `doc/pgsql.md`. The
`control_plane_dcs` step probes for all of them from each node before Patroni is
asked to start.

Existing clusters are not migrated. Switching a running cluster's DCS means
moving its Patroni state, which is a deliberate operation, not a config flip.
