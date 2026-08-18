# PostgreSQL HA with Patroni + etcd

This document covers what gets deployed on each VM, what the tech stack is, how the config files look, and how the cluster workflow operates.

See [doc/api.md](api.md) for the full API payload reference.  
**Diagram source (draw.io):** [diagrams/pgsql-cluster.drawio](diagrams/pgsql-cluster.drawio)

![PostgreSQL Patroni + etcd cluster — process flow for deploy, stop, and start/recover](assets/diagram/pgsql-cluster-flow.svg)

---

## Tech Stack

| Layer | Technology | Purpose |
|-------|-----------|---------|
| Database engine | PostgreSQL 14–18 | Data storage and SQL interface |
| HA manager | Patroni | Leader election, automatic failover, config management |
| Distributed consensus | etcd | Patroni DCS (Distributed Configuration Store) — stores cluster state |
| Health check | Patroni REST API (`:8008`) | Used by HAProxy to route to the current leader |
| Automation | Ansible playbooks | Deploy, configure, and manage the cluster lifecycle |

---

## What Is Installed on Each VM

Every cluster node (primary and standbys) runs the same stack:

```
DB Node VM
├── PostgreSQL server        (data engine; managed by Patroni, not systemd)
├── Patroni                  (HA manager; runs as patroni.service)
│   └── patroni.service      (systemd-managed; controls PostgreSQL process)
└── etcd                     (distributed key-value store for DCS)
    └── etcd.service         (systemd-managed)
```

**PostgreSQL is NOT managed by the distro `postgresql@...` systemd service after deploy.** Patroni takes over as the process manager. The distro service is stopped and disabled.

---

## Supported Topologies

| Mode | Nodes | Failover |
|------|-------|---------|
| Single-node | 1 primary | No |
| HA cluster | 1 primary + 1 or more standbys | Yes (automatic via Patroni) |

With the default per-node etcd, quorum needs an odd number of nodes: use **3 or
more** for production, and note that a 2-node cluster has no etcd quorum at all.

That constraint disappears with the shared control plane
(`SHARED_CONTROL_PLANE`): the DCS moves off the tenant's nodes, no etcd is
installed on them, and any node count — 1 and 2 included — is fully supported.
See [Shared control-plane DCS](#shared-control-plane-dcs) below.

---

## What Happens on Each VM After Deploy

### Phase 1 — Preflight
Each node is verified to have PostgreSQL, Patroni, etcd, and `postgres_exporter` binaries installed and reachable from the proxy. Unlike postgres/patroni/etcd, `postgres_exporter` has no distro package — it must be pre-installed on the node image at `/usr/local/bin/postgres_exporter` (override via `postgres_exporter_bin`). Preflight fails fast here if it's missing, rather than timing out later in the exporter-setup phase.

### Phase 2 — Base configuration
On each node:
- `postgresql@<version>` systemd service is stopped and disabled
- etcd systemd unit and config are written
- Patroni systemd unit is written

### Phase 3 — Node configuration
On each node, the playbook writes:
- `/etc/etcd/etcd.conf` — etcd member config (unique per node)
- `/etc/patroni/patroni.yml` — Patroni config (unique per node)

PostgreSQL data directory (`/var/lib/postgresql/<major>/main`) is cleared for a fresh bootstrap.

### Phase 4 — Cluster bootstrap
1. `etcd.service` starts on all nodes simultaneously
2. etcd cluster forms (peer discovery via `initial-cluster` list)
3. `patroni.service` starts on the **primary node**
4. Patroni bootstraps PostgreSQL, initializes the data directory, creates users
5. Patroni pushes `bootstrap.dcs` config to etcd:
   - `synchronous_mode: true`
   - `synchronous_mode_strict: false`
6. The API PATCH-es Patroni REST (`/config`) to enforce sync mode — idempotent, applies to both new and existing clusters
7. `patroni.service` starts on **standby nodes**
8. Each standby clones the primary data directory via `pg_basebackup` and starts streaming

### Phase 5 — Verification
- Patroni REST `/leader` confirms primary election
- Patroni REST `/replica` confirms all standbys are streaming
- `pg_stat_replication` row count matches `len(standby_ips)`
- Patroni cluster view confirms `sync_standby` is elected (when standbys > 0)

### Phase 6 — Application DB and user
If `new_db` and `new_user` are set:

When `new_user_superuser: true` (default):
```sql
CREATE ROLE appuser WITH LOGIN SUPERUSER CREATEDB CREATEROLE REPLICATION BYPASSRLS PASSWORD 'password';
CREATE DATABASE appdb OWNER appuser;
GRANT ALL PRIVILEGES ON DATABASE appdb TO appuser;
```

When `new_user_superuser: false`:
```sql
CREATE ROLE appuser WITH LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD 'password';
CREATE DATABASE appdb OWNER appuser;
GRANT ALL PRIVILEGES ON DATABASE appdb TO appuser;
```

`new_user_ssl_required` (default `true`) controls `pg_hba.conf` rules written by Patroni:
- `true` → `hostssl all appuser 0.0.0.0/0 scram-sha-256` + `hostnossl all appuser 0.0.0.0/0 reject`
- `false` → `host all appuser 0.0.0.0/0 scram-sha-256`

---

## Config Files Written to Each VM

### `/etc/etcd/etcd.conf`

```ini
ETCD_NAME=node1
ETCD_DATA_DIR=/var/lib/etcd
ETCD_LISTEN_CLIENT_URLS=http://0.0.0.0:2379
ETCD_ADVERTISE_CLIENT_URLS=http://10.0.0.1:2379
ETCD_LISTEN_PEER_URLS=http://0.0.0.0:2380
ETCD_INITIAL_ADVERTISE_PEER_URLS=http://10.0.0.1:2380
ETCD_INITIAL_CLUSTER=node1=http://10.0.0.1:2380,node2=http://10.0.0.2:2380,node3=http://10.0.0.3:2380
ETCD_INITIAL_CLUSTER_TOKEN=etcd-cluster-<cluster_name>
ETCD_INITIAL_CLUSTER_STATE=new
```

### `/etc/patroni/patroni.yml`

`max_connections` under `bootstrap.dcs.postgresql.parameters` only appears when the deploy request sets `connection_limit` (it is also PATCHed into the Patroni DCS config during `cluster_bootstrap`, so it applies cluster-wide); omitted, PostgreSQL's default of `100` applies.

`shared_buffers`/`effective_cache_size`/`maintenance_work_mem`/`work_mem` are derived from the node's detected RAM (`ansible_memtotal_mb`, gathered by the `runtime_facts` role) using standard pgtune/EDB heuristics, since plain PostgreSQL has no auto-tuning equivalent of MySQL's `innodb_dedicated_server`: `shared_buffers` = 25% of RAM (≈128MB at 512MB RAM, matching PostgreSQL's own built-in default — no regression on the smallest CloudStack plan), `effective_cache_size` = 75% of RAM, `maintenance_work_mem` = 10% of RAM capped at 1GB, `work_mem` = (RAM − shared_buffers) / (16 × vCPUs) floored at PostgreSQL's own 4MB default and capped at 256MB. Like `max_connections`, these only take effect at the cluster's *initial* bootstrap; they're computed per-node but only the value from whichever node actually bootstraps the DCS applies cluster-wide, so this assumes all nodes in a cluster share the same RAM (the normal case — one CloudStack offering per cluster).

```yaml
scope: pg-prod
namespace: /db/
name: node1

restapi:
  listen: 0.0.0.0:8008
  connect_address: 10.0.0.1:8008

etcd:
  hosts: 10.0.0.1:2379,10.0.0.2:2379,10.0.0.3:2379

bootstrap:
  dcs:
    ttl: 30
    loop_wait: 2
    retry_timeout: 10
    maximum_lag_on_failover: 1048576
    synchronous_mode: true
    synchronous_mode_strict: false
    postgresql:
      use_pg_rewind: true
      use_slots: true
      parameters:
        wal_level: replica
        hot_standby: "on"
        max_connections: 200
        shared_buffers: 8192MB
        effective_cache_size: 24576MB
        maintenance_work_mem: 1024MB
        work_mem: 192MB
        max_wal_senders: 10
        wal_keep_size: 1024
        password_encryption: scram-sha-256
        shared_preload_libraries: pg_stat_statements

  initdb:
    - encoding: UTF8
    - data-checksums

postgresql:
  listen: 0.0.0.0:5432
  connect_address: 10.0.0.1:5432
  data_dir: /var/lib/postgresql/16/main
  bin_dir: /usr/lib/postgresql/16/bin
  config_dir: /etc/postgresql/16/main
  pgpass: /tmp/pgpass

  authentication:
    superuser:
      username: postgres
      password: <postgres_password>
    replication:
      username: replicator
      password: <replicator_password>

  parameters:
    unix_socket_directories: /var/run/postgresql
    ssl: "on"
    ssl_cert_file: /etc/ssl/certs/ssl-cert-snakeoil.pem
    ssl_key_file: /etc/ssl/private/ssl-cert-snakeoil.key

tags:
  nofailover: false
  noloadbalance: false
  clonefrom: false
  nosync: false       ← all nodes eligible for sync_standby election
```

---

## Cluster Architecture

```
            Application Clients
                    │
                    ▼
          ┌──────────────────┐
          │     HAProxy       │
          │  :25042 (TCP)    │
          │  httpchk GET /leader on :8008 │
          │  → only current leader gets traffic │
          └────────┬─────────┘
                   │
       ┌───────────┼───────────┐
       │           │           │
       ▼           ▼           ▼
  ┌─────────┐ ┌─────────┐ ┌─────────┐
  │ Node 1  │ │ Node 2  │ │ Node 3  │
  │ Patroni │ │ Patroni │ │ Patroni │
  │ :8008   │ │ :8008   │ │ :8008   │
  │ [leader]│ │[sync_sb]│ │[replica]│
  └────┬────┘ └────┬────┘ └────┬────┘
       │ streaming replication  │
  ┌─────────┐ ┌─────────┐ ┌─────────┐
  │Postgres │ │Postgres │ │Postgres │
  │:5432    │◀│:5432    │ │:5432    │
  │(primary)│  streaming│  streaming│
  └─────────┘ └─────────┘ └─────────┘
       │           │           │
       └─────┬─────┘           │
             │    etcd DCS     │
       ┌─────▼───────────────────┐
       │  etcd cluster (3 nodes) │
       │  stores Patroni leader  │
       │  lock + cluster config  │
       └─────────────────────────┘
```

### Node roles

| Role | Description |
|------|-------------|
| `leader` | Current primary — accepts writes, holds the Patroni DCS lock |
| `sync_standby` | Synchronous standby — commits only confirmed by this node; RPO = 0 |
| `replica` | Asynchronous standby — small lag; does not block primary commits |

### Synchronous replication

`synchronous_mode: true` (DCS-managed) means:
- Patroni sets PostgreSQL `synchronous_standby_names` to the current `sync_standby` node
- Primary commits only after the `sync_standby` acknowledges the WAL write
- After a failover, Patroni automatically elects the remaining standby as the new `sync_standby`

`synchronous_mode_strict: false` means: if no sync_standby is available, the primary falls back to async mode (accepts writes) rather than blocking.

### HAProxy health check

HAProxy checks `GET http://<node>:8008/leader` on each backend. Patroni returns:
- `200 OK` — this node is the current leader (primary)
- `503` — this node is a standby or not ready

Only the leader receives client connections. After a failover, Patroni elects a new leader within seconds, and HAProxy reroutes automatically without any config change.

---

## Shared control-plane DCS

PostgreSQL has no quorum mechanism of its own — Patroni borrows one from etcd.
By default that etcd lives on the tenant's own database nodes (the box at the
bottom of the diagram above), which is why production needs 3+ nodes and why a
tenant losing one VM can cost it failover.

Setting `SHARED_CONTROL_PLANE` to a control-plane host's private IP moves the DCS
off the data plane. Clusters deployed from then on install **no etcd at all**:

```
  ┌─────────┐ ┌─────────┐            ┌────────────────────────────┐
  │ Node 1  │ │ Node 2  │  https +   │  shared control-plane etcd │
  │ Patroni ├─┤ Patroni ├──── auth ─▶│  /db/patroni/<cluster>/    │
  └─────────┘ └─────────┘            │  user patroni-<cluster>-…  │
       no local etcd                 └────────────────────────────┘
```

What each cluster is issued on the control plane:

| Object | Value |
|--------|-------|
| Role | `patroni-<cluster>-role`, `readwrite` on `/db/patroni/<cluster>/` |
| User | `patroni-<cluster>-user`, granted that role only |
| Keys | `/db/patroni/<cluster>/…` — leader lock, config, members |
| Client cert | `<engine>/<cluster>-user-<UTC stamp>.pem` on the control plane, **no CommonName** (see below) |

etcd RBAC is prefix-based, so tenants cannot read or write each other's keys.
Each node holds the control plane's CA (`/etc/patroni/etcd-ca.pem`), its tenant
certificate (`/etc/patroni/etcd-client.pem` + key) and its username/password —
never the root credential and never another tenant's material.

The client certificate is what a control plane running `client-cert-auth: true`
demands: it aborts the TLS handshake for a client presenting none, which shows
up on the node as `tlsv13 alert certificate required` and leaves Patroni looping
on `waiting on etcd`. Certificates are minted once per tenant from the CA and
left in place — a redeploy or start/recover adopts the pair already on disk and
never re-mints, so a live cluster cannot lose its credential to a routine
operation. Each minted pair is named for the UTC time it was issued, so a
cluster name re-used after a decommission gets a visibly distinct file instead
of one silently overwritten or inherited, and the directory dates every
credential on it. Rotation is deliberate: delete the pair under
`/etc/etcd/ssl/erawan-clients/` and re-run the deploy — the new pair carries the
time it was re-issued.
Set `CONTROL_PLANE_ETCD_CLIENT_CERT=false` for a control plane that does not
require client certificates.

The certificate deliberately carries **no CommonName**, and that is not
cosmetic — see [requirement 2](#2-tenant-client-certificates-must-carry-no-commonname)
below. A pair minted before that rule is detected and re-issued automatically on
the next `control_plane_dcs` run, so an affected cluster heals itself.

Certificates are filed per engine on the control plane:

```
/etc/etcd/ssl/erawan-clients/
  ca.srl                      one serial counter — X.509 serials are unique per CA
  pgsql/
    db-0001-user-20260818T110512Z.pem        minted 2026-08-18 11:05:12 UTC
    db-0001-user-20260818T110512Z-key.pem
```

The engine directory is what keeps two engines apart. Without it, a PostgreSQL
and a (say) Redis cluster both named `db-0001` mint to the same stem, and the
rule that adopts an existing pair silently hands the second engine the first
one's credential. `core.ControlPlane.ForEngine` derives the directory; the pre-existing
flat layout is migrated on the next provisioning run and removed on cleanup.

Note what `ForEngine` deliberately does **not** scope: the etcd user
(`patroni-<cluster>-user`) and the key namespace (`/db/patroni/<cluster>/`).
Both are written into a node's `patroni.yml`, and only a full deploy rewrites
that file — renaming them would leave running nodes authenticating as a user
nothing converges any more, and would point the next redeploy at an empty
keyspace it would then bootstrap over live data. A second engine must be given
its own prefix and namespace explicitly via `CONTROL_PLANE_DCS_TENANT_PREFIX`
and `CONTROL_PLANE_DCS_NAMESPACE`, which is a deployment decision, not a
derivation.

**Lifecycle.** The tenant namespace is created by the `control_plane_dcs` step,
which runs on every deploy, start/recover and add-member and is idempotent — it
is also what re-installs the CA and re-opens the control plane's firewall for a
node that a scale operation rebuilt with a new IP. Removing a member revokes that
node's access. `DELETE /cluster/pgsql/dcs` releases the whole tenant (keys, user,
role, firewall grants) when a cluster is decommissioned; nothing else does, and
the objects are named after the cluster, so a later cluster of the same name
would otherwise inherit them.

### Configuring the control plane

Erawan **consumes** the control plane and never provisions it: it creates and
deletes per-tenant roles, users, key prefixes and firewall grants, and nothing
else. Standing up the host — etcd, TLS material, the root user, `auth enable` —
is yours. Erawan reaches it over SSH with the cluster's own key by default.

Four requirements below are load-bearing, and none of them is caught by the
obvious `curl https://<cp>:2379/version` smoke test — that endpoint is served by
etcd's own HTTP handler and answers even when the API Patroni actually uses is
completely unavailable. All four were verified against etcd 3.4.30.

#### Reference `/etc/etcd/etcd.conf.yml`

```yaml
name: cp-etcd-01
data-dir: /var/lib/etcd

listen-peer-urls: https://10.10.3.66:2380
listen-client-urls: https://10.10.3.66:2379,https://127.0.0.1:2379

initial-advertise-peer-urls: https://10.10.3.66:2380
advertise-client-urls: https://10.10.3.66:2379

initial-cluster: cp-etcd-01=https://10.10.3.66:2380
initial-cluster-state: new
initial-cluster-token: erawan-shared-cp

enable-grpc-gateway: true          # requirement 1 — NOT the default here

client-transport-security:
  cert-file: /etc/etcd/ssl/cp-etcd-01.pem
  key-file: /etc/etcd/ssl/cp-etcd-01-key.pem
  trusted-ca-file: /etc/etcd/ssl/ca.pem
  client-cert-auth: true

peer-transport-security:
  cert-file: /etc/etcd/ssl/cp-etcd-01.pem
  key-file: /etc/etcd/ssl/cp-etcd-01-key.pem
  trusted-ca-file: /etc/etcd/ssl/ca.pem
  peer-client-cert-auth: true
```

#### 1. The gRPC JSON gateway must be enabled

Patroni's `etcd3` client speaks etcd v3 over **HTTP** (`POST /v3/...`), never
gRPC directly. Those paths exist only when the gateway is on — and etcd defaults
that setting differently depending on how the process starts:

| Started with | `enable-grpc-gateway` default |
|--------------|-------------------------------|
| command-line flags | `true` |
| `--config-file` | **`false`** |

The flag is registered with a `true` default in `etcdmain/config.go`, but the
struct field it fills is a plain bool in `embed/config.go` that `NewConfig()`
never sets — so a config-file deployment that merely *omits* the key runs with
the gateway off. Nothing warns you. Set it explicitly, as above.

With the gateway off, `/version` still answers and `etcdctl` still works (it
speaks gRPC on the same port), so the `control_plane_dcs` step provisions the
tenant successfully. Then every Patroni call receives a plain-text
`404 page not found` body, which Patroni JSON-decodes into the integer `404`
and dies on:

```
AttributeError: 'int' object has no attribute 'get'
```

#### 2. Tenant client certificates must carry no CommonName

When `client-cert-auth`, the gateway and RBAC auth are **all three** on, etcd
rejects every `/v3` request whose client certificate has a non-empty CN:

```
HTTP 400  CommonName of client sending a request against gateway
          will be ignored and not used as expected
```

The guard is in etcd's `embed/serve.go`: CN-derived identity cannot be conveyed
across the gateway, so rather than silently ignoring it, etcd refuses the
request. Erawan therefore mints tenant certificates with subject `/O=erawan` and
no CN — the tenant is identified by its RBAC username and password instead, and
prefix isolation is unaffected.

#### 3. The control plane's server certificate needs `clientAuth`

The gateway proxies inbound HTTP to etcd's own gRPC listener, connecting to
itself as a client using the server certificate. With `client-cert-auth: true`
that certificate must therefore carry **both** extended key usages, or every
`/v3` request returns HTTP 503 `error reading server preface: remote error:
tls: bad certificate`:

```bash
openssl x509 -in /etc/etcd/ssl/cp-etcd-01.pem -noout -ext extendedKeyUsage
# TLS Web Server Authentication, TLS Web Client Authentication
```

The certificate must also carry the control plane's IP in its SANs: Patroni
verifies it against the IP it dials, and `control_plane_dcs` fails by name on
that rather than letting the cluster start and fail every DCS call later.

A loopback entry in `listen-client-urls` (`https://127.0.0.1:2379` above) is what
the gateway dials, so keep it.

#### 4. RBAC must be enabled

Prefix-scoped RBAC is the only thing keeping tenants off each other's keys.
Erawan creates per-tenant roles and users but never runs `auth enable`:

```bash
etcdctl user add root --interactive=false --new-user-password='<root-pw>'
etcdctl auth enable
```

That password is what `CONTROL_PLANE_ETCD_ROOT_PASSWORD` must match. If auth is
left off, deploys still succeed — Patroni tolerates it — but every tenant can
read and write every other tenant's Patroni state. The `control_plane_dcs` step
emits a warning in that case rather than failing.

#### Verifying

Run from the control plane after any change. The `control_plane_dcs` step runs
the same check from each database node and fails the deploy with the cause named,
so this is a pre-flight, not the only line of defence:

```bash
curl -s -o /dev/null -w '%{http_code}\n' \
  --cacert /etc/etcd/ssl/ca.pem \
  --cert /etc/etcd/ssl/cp-etcd-01.pem \
  --key /etc/etcd/ssl/cp-etcd-01-key.pem \
  -X POST https://10.10.3.66:2379/v3/auth/authenticate -d '{}'
```

`404` means the gateway is off. Anything else means it is serving — including
`400`, which here is just requirement 2 firing on the server certificate's own
CN and confirms the gateway is live.

#### Symptom index

| Seen on the node | Cause |
|------------------|-------|
| `AttributeError: 'int' object has no attribute 'get'` in the Patroni journal | Gateway off — requirement 1 |
| HTTP 400 `CommonName ... will be ignored` | Tenant certificate has a CN — requirement 2 |
| HTTP 503 `error reading server preface: ... tls: bad certificate` | Server certificate lacks `clientAuth` — requirement 3 |
| `tlsv13 alert certificate required` | Node has no client certificate; check `CONTROL_PLANE_ETCD_CLIENT_CERT` |
| `certificate verify failed` / IP address mismatch | Control-plane IP missing from the server certificate's SANs |
| Deploy warns `authentication is not enabled` | `auth enable` never run — requirement 4 |
| `invalid user ID or password` | Tenant user dropped out of band, or the job's DCS password no longer matches |

Changing any of the above needs an etcd restart, which briefly drops the DCS for
**every** tenant on the control plane. Patroni rides out a short outage without
failing over, but time it deliberately.

**Existing clusters are never migrated.** Each job records the DCS layout it was
deployed with, so turning the variable on or off cannot re-point a live cluster.

---

## Failover Flow

```
1. Primary node (Node 1) dies or loses etcd lock
           │
           ▼
2. Patroni on Node 2 (sync_standby) detects leader loss
   → tries to acquire etcd lock
           │
           ▼
3. Node 2 wins election → becomes leader
   → promotes PostgreSQL to read-write
   → Node 2 REST /leader now returns 200
           │
           ▼
4. HAProxy health check fires (≤10s interval)
   → Node 1: /leader returns 503 (or no response)
   → Node 2: /leader returns 200
   → HAProxy reroutes all new connections to Node 2
           │
           ▼
5. Node 3 (was replica) detects new leader via etcd
   → Patroni elects Node 3 as sync_standby
   → pg_rewind used if Node 3 diverged during transition
           │
           ▼
6. Cluster stable: Node 2=leader, Node 3=sync_standby
   (Node 1 rejoins as replica when it recovers)
```

---

## Add Member Workflow

```
POST /cluster/pgsql/members
  { "job_id": "abc", "member_ips": ["10.0.0.4", "10.0.0.5"] }
           │
           ▼   (members join ONE AT A TIME, in order — etcd allows a
           │    single unpromoted learner; each success is added to the
           │    standby list before the next join starts)
  0. Clean up stale etcd registrations left by destroyed nodes; if dead
     stale voters already cost the primary its quorum, recover etcd with
     force-new-cluster (only when every other voter is stale AND
     unreachable — otherwise the job fails with a reason)
  1. Register new node as etcd learner
  2. Configure etcd on new node (joins existing cluster)
  3. Promote etcd learner to full voting member
  4. Write /etc/patroni/patroni.yml on new node
  5. Start patroni.service on new node
  6. Patroni clones data from primary via pg_basebackup
  7. New node starts streaming replication
  8. Patroni verifies /replica API returns 200
  9. If no sync_standby exists, new node may be elected sync_standby
```

On failure the job stops at the failing member; nodes that already joined
stay in the cluster. Retry with only the remaining IPs.

---

## Stop / Start Workflow

Stop and start the whole cluster without losing data (planned maintenance,
VM resizing, etc.).

```
POST /cluster/pgsql/jobs/{jobID}/stop
  1. systemctl stop patroni on all standbys   (primary keeps serving)
  2. systemctl stop patroni on the primary    (clean shutdown, WAL flushed)
  3. Force-stop any PostgreSQL cluster still running on any node
     (pg_lsclusters + pg_ctlcluster stop -m fast) — Patroni's own shutdown
     of postgres isn't always reliable, so this is a belt-and-suspenders
     check on every node before etcd goes down
  4. systemctl stop etcd on all nodes         (after every Patroni/postgres is down)

POST /cluster/pgsql/jobs/{jobID}/start        (alias: /recover)
  1. cluster_bootstrap (re-run, idempotent) on every node, before etcd starts:
     a. Kill any etcd process not tracked by systemd — a node that froze
        or was power-cycled can leave an orphaned etcd holding the data
        directory's file lock, which makes systemd's own restart loop
        crash-loop forever ("cannot lock data directory")
     b. Refresh the `initial-cluster` peer list in etcd.conf to the
        cluster's current membership. A node's config is only written once
        (deploy, or add-member for a joined node) and never touched again
        when membership later changes elsewhere — so a stale peer count
        makes etcd reject startup with "member count is unequal" the next
        time that node restarts. Rewritten unconditionally every run from
        the same live inventory Ansible was given, independent of how many
        add/remove-member operations happened in between
     c. Force `initial-cluster-state: new` on nodes whose data directory is
        already initialized — skips a remote-peer validation that only
        matters for a brand-new node's first join
     d. Start etcd, then Patroni (primary first, then standbys) — Patroni
        starts postgres itself via pg_ctl against the existing data
        directory and each node re-registers/rejoins via the DCS. An etcd
        or Patroni bootstrap failure automatically captures the last 80
        journal lines for that service so the real error is visible in the
        job's step output, not just a bare timeout
  2. verify_cluster: poll Patroni cluster status until it reports healthy
     and every expected node is online (retries with backoff) — the job
     fails if the cluster doesn't converge, instead of a false success
```

See [assets/diagram/pgsql-cluster-flow.svg](assets/diagram/pgsql-cluster-flow.svg) for the full step-by-step flow including deploy.

Data directories (PostgreSQL and etcd) are never touched by either
operation. Stop is rejected while a deploy or member operation is running
on the same cluster. The same VMs/disks must come back for start — start
does not rebuild destroyed nodes (use add-member for that).

---

## Metrics Collection

Metrics are collected from **Prometheus exporters** and the **Patroni REST API** running on each node — no database credentials or direct SQL connections are needed:

```
POST /cluster/pgsql/metrics
  { "job_id": "abc", "proxy_port": 25042 }
           │
           ▼
  Resolve node_ips from stored job
           │
           ▼
  Scrape postgres_exporter :9187 on each node (parallel)
  Scrape node_exporter :9100 on each node (parallel)
  Call Patroni REST :8008 on each node (cluster/failover categories)
           │
           ▼
  Discover primary from Patroni leader API
           │
           ▼
  Aggregate per-category metrics, return JSON
```

`postgres_exporter` and `node_exporter` must be running on every DB node. The API contacts exporters and Patroni directly on node IPs — HAProxy is not in the metric path.

---

## Important Behaviors

- PostgreSQL data directory and etcd data are cleared **once per deployment job**. Resuming a failed job does not clear data again.
- `pg_rewind` is enabled — this allows a former primary that diverged to rejoin as a standby without a full data copy.
- `pg_stat_statements` is loaded via `shared_preload_libraries` and available immediately after deploy.
- `password_encryption: scram-sha-256` is enforced cluster-wide — MD5 auth is not supported.
- SSL is enabled using the distro default snakeoil certificate. Replace with a real cert for production.
- Single-node mode: `standby_ips: []` — only primary is bootstrapped. No HA.
- The deploy response credentials (`postgres_password`, `replicator_password`, `admin_password`) are stored per-job and used automatically when `job_id` is supplied to the metrics endpoint.
- Multiple `member_ips` in one add-member request join sequentially, never in parallel — parallel joins race on etcd learner registration and can remove each other's in-progress registration.
- Removing a node from the cluster must go through the remove-member API. Destroying a VM without it leaves a dead voter registered in etcd; enough dead voters cost the primary its quorum and wedge etcd (`unhealthy cluster` / `context deadline exceeded`). The next add-member run auto-recovers this when provably safe, but remove-member first is always the cleaner path.
- Stop/start (and recover) never touch data directories; a stopped cluster restarts from its existing PostgreSQL and etcd data.
- Every node's `etcd.conf` peer list is re-synced to the cluster's actual current membership on every `start`/`recover`, not just the node that most recently joined via add-member — this is what makes stop/start safe to run any number of add/remove-member operations after the original deploy.
- An etcd or Patroni failure during `start`/`recover` captures that service's last 80 journal lines directly into the failed job's step output — check `GET /jobs/{jobID}` before reaching for SSH.
- Ansible connections use pipelining and a pinned Python interpreter, cutting per-module SSH round-trips on every host and step.
