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

Each cluster (tenant) is issued its own etcd role, user, key prefix and client
certificate on the shared control plane:

```
role  patroni-<cluster>-role   readwrite on /db/patroni/<cluster>/
user  patroni-<cluster>-user   granted that role and nothing else
keys  /db/patroni/<cluster>/…  Patroni's leader, config, members, …
cert  <engine>/<cluster>-user-<UTC stamp>.pem, subject /O=erawan (no CN)
```

etcd RBAC is prefix-based, which is what keeps tenants off each other's keys on
infrastructure they all share. The DB nodes hold the control plane's CA, their
own tenant certificate and their own username/password — never the root
credential, never another tenant's material, and no reach outside their prefix.

The client certificate exists because a control plane running
`client-cert-auth: true` aborts the TLS handshake for a client that presents
none, long before any username/password is exchanged. Each tenant gets its own
so it can be revoked independently.

It carries **no CommonName**, which is load-bearing rather than an omission.
With `client-cert-auth`, the gRPC JSON gateway and RBAC auth all enabled, etcd
rejects every `/v3` request whose client certificate has a CN — `HTTP 400
CommonName of client sending a request against gateway will be ignored and not
used as expected`, from the guard in etcd's `embed/serve.go`. CN-derived
identity cannot cross the gateway, and Patroni reaches the DCS only over that
gateway, so a CN fails the whole cluster rather than one call. The tenant is
identified by its RBAC username and password instead; prefix isolation is
unaffected. A pair minted before this rule is detected by its subject and
re-issued on the next provisioning run, so affected tenants heal themselves.

Certificates are filed per engine — `dcs_client_cert_dir` is
`/etc/etcd/ssl/erawan-clients/<engine>/`, derived by
`core.ControlPlane.ForEngine`. That directory is what keeps engines apart: two
engines each deploying a cluster named `db-0001` would otherwise mint to the
same stem, and the adopt-if-present rule below would hand the second engine the
first one's credential. The CA serial counter stays one level up, shared —
X.509 serial numbers must be unique per CA, not per directory.

`ForEngine` deliberately leaves the etcd user prefix and key namespace alone.
Both are baked into every deployed cluster's engine config, which only a full
deploy rewrites, so deriving them from the engine name would strand running
nodes on a user nothing converges and re-point the next redeploy at an empty
keyspace. A second engine gets its own via `dcs_namespace` /
`dcs_tenant_user`, as a deployment decision.

A minted pair is stamped with the UTC time it was issued —
`<cluster>-user-<YYYYMMDDTHHMMSSZ>.pem`, key beside it as `…-<stamp>-key.pem`.
Minting is still one-time per tenant: provisioning adopts the newest complete
pair already in the directory and only mints when there is none, so the routine
re-runs below never hand a live cluster a new credential. What the stamp buys is
that two incarnations of one cluster name cannot occupy the same filename — a
tenant re-created after a cleanup is visibly a new credential rather than a file
silently overwritten, and the directory says when each was issued. Rotation
still means deleting the pair under `dcs_client_cert_dir` and re-running; the
re-issued pair carries the new time. Cleanup removes every pair matching
`<cluster>-user-*`, so a tenant that was re-issued at some point leaves none
behind. Set
`dcs_client_cert_enabled: false` for a control plane that does not require them
— it is also the only setting that needs the CA private key on the control
plane.

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
- `etcd_dcs_client` — the node-side half: installs the CA and the tenant
  certificate, then proves the node can actually use the control plane. Two
  probes, both deliberate. `GET /version` validates the server certificate
  **against the IP the node dials** — without that IP in its SANs a cluster
  starts and then fails every DCS call with a TLS error buried in the engine's
  journal. `POST /v3/auth/authenticate`, with the tenant's own certificate and
  credential, then covers everything `/version` cannot see: the gateway being
  off, a CN on the certificate, a server certificate missing `clientAuth`, RBAC
  being disabled, and a credential the control plane no longer accepts.

## What is assumed, and what is not

The control plane itself — its etcd, TLS material, root user and `auth enable` —
is expected to exist already. Erawan consumes it and never provisions it, which
is why the roles fail early and by name when `/etc/etcd/ssl/*.pem` is missing or
the root credential is wrong.

Four settings on that host are load-bearing, and none of them is visible to a
`GET /version` check — which is served by etcd's own HTTP handler and answers
even when the API the engines use is entirely unavailable:

1. **`enable-grpc-gateway: true`.** Engines reach etcd v3 over HTTP, not gRPC.
   etcd defaults this to `true` for command-line flags but **`false`** when
   started with `--config-file`, so a config-file host that omits the key serves
   no `/v3` at all — while `etcdctl` and `/version` keep working, which is what
   makes it so easy to miss.
2. **Tenant certificates with no CommonName** — see above.
3. **A server certificate carrying both `serverAuth` and `clientAuth`**, plus the
   control plane's IP in its SANs. The gateway dials etcd's own gRPC listener as
   a client using this certificate, so `clientAuth` is not optional under
   `client-cert-auth`.
4. **`auth enable`**, without which prefix RBAC is inert and tenants are not
   isolated from one another.

`etcd_dcs_client` probes for all four from each node and fails the run with the
cause named. `doc/pgsql.md` carries a reference config, the verification command
and a symptom index.

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
