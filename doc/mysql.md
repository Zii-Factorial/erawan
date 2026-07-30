# MySQL InnoDB Cluster

This document covers what gets deployed on each VM, what the tech stack is, how the config files look, and how the cluster workflow operates.

See [doc/api.md](api.md) for the full API payload reference.  
**Diagram source (draw.io):** [diagrams/mysql-cluster.drawio](diagrams/mysql-cluster.drawio)

![MySQL InnoDB Cluster — process flow for deploy, stop, and start/recover](assets/diagram/mysql-cluster-flow.svg)

---

## Tech Stack

| Layer | Technology | Purpose |
|-------|-----------|---------|
| Database engine | MySQL 8 or 9 | Data storage and SQL interface |
| Cluster layer | MySQL InnoDB Cluster (Group Replication) | HA, automatic failover, distributed writes |
| Cluster management | MySQL Shell (`mysqlsh`) | Configure instances, create cluster, add/remove members |
| Connection routing | HAProxy + primary-check endpoint | HAProxy routes writes to whichever node's primary-check reports PRIMARY |
| Automation | Ansible playbooks | Deploy, configure, and manage the cluster lifecycle |

---

## What Is Installed on Each VM

All cluster nodes (primary and secondaries) get the same software:

```
DB Node VM
├── MySQL Server         (data + SQL engine)
│   └── Group Replication (cluster consensus layer, built-in)
├── MySQL Shell (mysqlsh) (used by automation to manage the cluster)
└── primary-check :9200  (tiny HTTP endpoint used by HAProxy's httpchk)
    └── erawan-mysql-primary-check.service (systemd-managed)
```

### Primary-check endpoint

This deployment does not use MySQL Router. Instead, every DB node runs a small
HTTP endpoint (`erawan-mysql-primary-check.service`, port `9200` by default)
that answers `GET /` with `200` when the local node is the current Group
Replication primary and `503` otherwise, based on
`performance_schema.replication_group_members`.

HAProxy's `httpchk` polls this endpoint on every node (see
`BuildMySQLConfig` in `internal/haproxy/service.go`) and routes connections to
whichever node currently answers `200` — the same pattern used for Patroni's
`/leader` endpoint on the PostgreSQL side. This handles both crashes (node
unreachable) and planned switchovers (old primary starts answering `503` as
soon as GR demotes it).

---

## Supported Topologies

| Mode | Nodes | Failover |
|------|-------|---------|
| Single-node | 1 primary | No |
| HA cluster | 1 primary + 1 or more secondaries | Yes (auto) |

For automatic failover and quorum, use **at least 3 nodes** (Group Replication needs a majority vote).

---

## What Happens on Each VM After Deploy

### Phase 1 — Preflight checks
Each node is verified to have:
- MySQL running
- `mysqlsh` installed
- Local root access via Unix socket
- Network connectivity to other nodes on `mysql_port`

### Phase 2 — Instance configuration
On each node via `mysqlsh`:
- `dba.configureInstance()` runs to set Group Replication prerequisites in `my.cnf`
- The cluster admin user is created or confirmed
- Auto-rejoin settings are written:
  - `group_replication_start_on_boot = ON`
  - `group_replication_autorejoin_tries = 3`
  - `group_replication_unreachable_majority_timeout = 30`
  - `group_replication_exit_state_action = READ_ONLY`

### Phase 3 — Cluster creation (primary node)
On the primary:
```
mysqlsh -- dba createCluster prodCluster
```
A single-node InnoDB Cluster is created. Group Replication starts.

### Phase 4 — Add secondaries
For each `standby_ip`:
```
mysqlsh -- cluster addInstance 10.0.0.2:3306 --recoveryMethod=auto
```
`recoveryMethod=auto` prefers incremental recovery (fast) and falls back to MySQL Clone (full data copy) when needed.

### Phase 5 — Primary-check endpoint
On every node, `erawan-mysql-primary-check.service` is installed and started,
listening on `primary_check_port` (default `9200`). HAProxy uses this to find
the current primary — see [Primary-check endpoint](#primary-check-endpoint).

### Phase 6 — Application DB and user
If `new_db` and `new_user` are set, these are created on the primary via `mysqlsh`.

When `new_user_superuser: true` (default):
```sql
CREATE USER 'appuser'@'%' IDENTIFIED BY 'password';
GRANT ALL PRIVILEGES ON *.* TO 'appuser'@'%' WITH GRANT OPTION;
-- + GRANT <dynamic_priv> ON *.* ... for each of 36 dynamic privileges
```

When `new_user_superuser: false`:
```sql
CREATE USER 'appuser'@'%' IDENTIFIED BY 'password';
GRANT ALL PRIVILEGES ON `appdb`.* TO 'appuser'@'%';
```

`new_user_ssl_required: true` adds `REQUIRE SSL` to the `ALTER USER` statement.

---

## Files on Each Node

After a successful deploy, every node has the following files and services installed. Examples below use node IPs `10.0.0.1 / 10.0.0.2 / 10.0.0.3`, cluster name `prod`, and MySQL port `3306`.

### Summary

| File / Path | All nodes | Differs per node |
|-------------|-----------|-----------------|
| `/etc/mysql/mysql.conf.d/99-erawan-cluster.cnf` | yes | `report_host`, `server_id` |
| `/var/lib/mysql/mysqld-auto.cnf` | yes | `group_replication_local_address` |
| `/etc/erawan-cluster/mysql-recovery.json` | yes | no |
| `/usr/local/bin/erawan-mysql-boot-recovery` | yes | no |
| `/etc/systemd/system/erawan-mysql-boot-recovery.service` | yes | `BOOT_RECOVERY_DELAY_SECONDS` (30 / 60 / 90) |
| `/usr/local/bin/erawan-gr-watchdog` | yes | no |
| `/etc/systemd/system/erawan-gr-watchdog.service` | yes | no |
| `/etc/systemd/system/erawan-gr-watchdog.timer` | yes | no |
| `/usr/local/bin/erawan-mysql-primary-check` | yes | no |
| `/etc/systemd/system/erawan-mysql-primary-check.service` | yes | no |

---

### `/etc/mysql/mysql.conf.d/99-erawan-cluster.cnf`

Group Replication configuration. Written by the `configure_instances` role. `report_host` and `server_id` are unique per node. When the deploy request sets `connection_limit`, a `max_connections = <connection_limit>` line is added (the `auto_rejoin` role also applies it live via `SET PERSIST`, so it takes effect even on `assume_prepared` deploys that skip this file).

**Node 1 (primary)**
```ini
[mysqld]
bind-address = 0.0.0.0
report_host = 10.0.0.1
max_connections = 500
mysqlx = OFF
loose-group_replication_start_on_boot = OFF
loose-group_replication_autorejoin_tries = 10
loose-group_replication_member_expel_timeout = 5
loose-group_replication_unreachable_majority_timeout = 60
loose-group_replication_exit_state_action = READ_ONLY
loose-group_replication_communication_stack = MYSQL
loose-group_replication_message_cache_size = 134217728
loose-group_replication_consistency = BEFORE_ON_PRIMARY_FAILOVER
relay_log_recovery = ON
server_id = 1
enforce_gtid_consistency = ON
gtid_mode = ON
binlog_transaction_dependency_tracking = WRITESET
log_bin = mysql-bin
binlog_format = ROW
ssl_ca = /var/lib/mysql/ca.pem
ssl_cert = /var/lib/mysql/server-cert.pem
ssl_key = /var/lib/mysql/server-key.pem
```

**Node 2**: `report_host = 10.0.0.2`, `server_id = 2` — all other lines identical  
**Node 3**: `report_host = 10.0.0.3`, `server_id = 3` — all other lines identical

> `loose-group_replication_start_on_boot = OFF` in this file is intentional for fresh deploys. The `auto_rejoin` role writes `SET PERSIST group_replication_start_on_boot = ON` to `/var/lib/mysql/mysqld-auto.cnf` which takes precedence at runtime.

> `group_replication_message_cache_size` (`mysql_gr_message_cache_size` var, default `134217728` = 128MB) is set to the MySQL-enforced minimum (MySQL >= 8.0.21; below that the floor is 1GB) to fit the smallest CloudStack plan (512MB RAM). The cache structures need ~50MB beyond the configured value, so budget ~180MB for GR alone on a 512MB node. Raise this var for larger plans if XCom cache eviction becomes an issue under heavy write load.

Verify: `sudo cat /etc/mysql/mysql.conf.d/99-erawan-cluster.cnf`

---

### `/var/lib/mysql/mysqld-auto.cnf` (persisted runtime variables)

Written by `SET PERSIST` statements during deploy. Overrides the `.cnf` file above. **Do not edit manually** — MySQL manages this file.

**Shared values written on all nodes** (by `configure_instances` and `auto_rejoin` roles):
```json
{
  "Version": 1,
  "mysql_server": {
    "group_replication_start_on_boot":                {"Value": "ON",       "Metadata": {"Timestamp": ...}},
    "group_replication_autorejoin_tries":             {"Value": "10",       "Metadata": {"Timestamp": ...}},
    "group_replication_member_expel_timeout":         {"Value": "5",        "Metadata": {"Timestamp": ...}},
    "group_replication_unreachable_majority_timeout": {"Value": "60",       "Metadata": {"Timestamp": ...}},
    "group_replication_exit_state_action":            {"Value": "READ_ONLY","Metadata": {"Timestamp": ...}},
    "group_replication_communication_stack":          {"Value": "MYSQL",    "Metadata": {"Timestamp": ...}},
    "require_secure_transport":                       {"Value": "OFF",      "Metadata": {"Timestamp": ...}}
  }
}
```

**Node-specific additions** (written by MySQL Shell `cluster.addInstance()`):

| Variable | Node 1 | Node 2 | Node 3 |
|----------|--------|--------|--------|
| `group_replication_local_address` | `10.0.0.1:33061` | `10.0.0.2:33061` | `10.0.0.3:33061` |
| `group_replication_group_seeds` | `10.0.0.1:33061,10.0.0.2:33061,10.0.0.3:33061` | same | same |
| `group_replication_group_name` | cluster UUID (same on all nodes) | same | same |

Verify: `sudo cat /var/lib/mysql/mysqld-auto.cnf`  
Verify live values: `mysql -uroot -e "SELECT variable_name, variable_value FROM performance_schema.persisted_variables ORDER BY variable_name;"`

---

### `/etc/erawan-cluster/mysql-recovery.json` (permissions: 0600)

Cluster admin credentials used by both the boot recovery script and the GR watchdog. Written by the `boot_recovery` role. Identical on all nodes.

```json
{
    "admin_pass": "<cluster_admin_password>",
    "admin_user": "clusteradmin",
    "cluster_name": "prod",
    "mysql_port": 3306,
    "node_ips": [
        "10.0.0.1",
        "10.0.0.2",
        "10.0.0.3"
    ]
}
```

Verify: `sudo cat /etc/erawan-cluster/mysql-recovery.json`

---

### `/usr/local/bin/erawan-mysql-boot-recovery` (permissions: 0755)

Runs in two different ways:

- **At boot** (unattended): the systemd service below fires on every node with staggered delays (30/60/90s). The first node to wake calls `dba.reboot_cluster_from_complete_outage()`; the others detect the cluster is already active and call `cluster.rejoinInstance()` to join it.
- **On demand**: `POST /cluster/mysql/start` (or `/recover`) runs `systemctl start mysql` on every node, then runs this script **once, on the primary only, with `BOOT_RECOVERY_DELAY_SECONDS=0`**. `reboot_cluster_from_complete_outage()` rejoins every reachable member itself, so no per-node race is needed; `verify_cluster` (with retries) then confirms all members reached ONLINE, and the GR watchdog picks up any straggler. If the cluster is already online, the script exits immediately — on a fresh deploy this step is a ~2-second no-op.

The `mysqlsh` call is wrapped in `timeout 300` (`boot_recovery_mysqlsh_timeout_seconds`), and a genuine failure propagates as a real exit code — previously the script always exited 0, so a failed recovery could be silently reported as a success.

**Same script on all nodes.** The staggered delay comes from `BOOT_RECOVERY_DELAY_SECONDS` in the service unit.

Verify: `sudo cat /usr/local/bin/erawan-mysql-boot-recovery`  
Run manually: `sudo BOOT_RECOVERY_DELAY_SECONDS=0 /usr/local/bin/erawan-mysql-boot-recovery`  
Logs: `sudo journalctl -u erawan-mysql-boot-recovery -n 100`

---

### `/etc/systemd/system/erawan-mysql-boot-recovery.service`

One-shot systemd service. `BOOT_RECOVERY_DELAY_SECONDS` is the only value that differs per node — it staggers boot recovery so only the first node bootstraps the cluster.

**Node 1 (primary)**
```ini
[Unit]
Description=Erawan MySQL boot recovery — reboot InnoDB Cluster after complete outage
After=mysql.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
Environment=BOOT_RECOVERY_DELAY_SECONDS=30
ExecStart=/usr/local/bin/erawan-mysql-boot-recovery
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

**Node 2**: `BOOT_RECOVERY_DELAY_SECONDS=60`  
**Node 3**: `BOOT_RECOVERY_DELAY_SECONDS=90`

Why staggered: node 1 fires at T+30s and bootstraps the cluster. Node 2 fires at T+60s, finds the cluster active, and rejoins. Node 3 fires at T+90s and does the same. This prevents split-brain from all 3 nodes trying to become primary simultaneously.

Verify: `sudo systemctl cat erawan-mysql-boot-recovery`  
Status: `sudo systemctl status erawan-mysql-boot-recovery`

---

### `/usr/local/bin/erawan-gr-watchdog` (permissions: 0755)

Fires every 60 seconds via the timer. Checks this node's GR state by querying `performance_schema.replication_group_members WHERE member_host = @@report_host`. If not ONLINE or RECOVERING, it:

1. Stops GR and resets the applier channel
2. Connects to each peer node (from `mysql-recovery.json`) and calls `cluster.rejoinInstance()`
3. Falls back to raw `START GROUP_REPLICATION` if no peer is reachable

**Same script on all nodes.**

Verify: `sudo cat /usr/local/bin/erawan-gr-watchdog`  
Trigger manually: `sudo /usr/local/bin/erawan-gr-watchdog`  
Logs: `sudo journalctl -t erawan-gr-watchdog -n 100`

---

### `/etc/systemd/system/erawan-gr-watchdog.service`

```ini
[Unit]
Description=Erawan GR watchdog — auto-heal stuck Group Replication applier
After=mysql.service

[Service]
Type=oneshot
ExecStart=/usr/local/bin/erawan-gr-watchdog
```

Same on all nodes. Run by the timer, not directly.

---

### `/etc/systemd/system/erawan-gr-watchdog.timer`

```ini
[Unit]
Description=Run erawan-gr-watchdog every 60 seconds

[Timer]
OnBootSec=120
OnUnitActiveSec=60s
AccuracySec=5s

[Install]
WantedBy=timers.target
```

Same on all nodes. `OnBootSec=120` delays the first watchdog run to 2 minutes after boot — allowing boot recovery to finish first.

Status: `sudo systemctl status erawan-gr-watchdog.timer`  
Next run: `sudo systemctl list-timers erawan-gr-watchdog.timer`

---

### `/usr/local/bin/erawan-mysql-primary-check` (permissions: 0755)

A small Python `http.server` script. On every `GET` request it queries
`performance_schema.replication_group_members` via the local root socket for
this node's `MEMBER_ROLE` and responds `200` if `PRIMARY`, `503` otherwise
(including when GR isn't running yet). Stateless — no caching, so it always
reflects live GR role. **Same script on all nodes.**

Verify: `sudo cat /usr/local/bin/erawan-mysql-primary-check`  
Test: `curl -i http://127.0.0.1:9200/`

---

### `/etc/systemd/system/erawan-mysql-primary-check.service`

```ini
[Unit]
Description=Erawan MySQL primary-check HTTP endpoint (GR role for HAProxy)
After=mysql.service
Requires=mysql.service

[Service]
Type=simple
ExecStart=/usr/bin/python3 /usr/local/bin/erawan-mysql-primary-check
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

Same on all nodes.

Status: `sudo systemctl status erawan-mysql-primary-check`  
Logs: `sudo journalctl -u erawan-mysql-primary-check -n 100`

---

### Cluster state verification commands

Run these on any node to inspect the current cluster state:

```bash
# Check this node's GR membership state
mysql -uroot -e "SELECT member_host, member_port, member_state, member_role FROM performance_schema.replication_group_members;"

# Check all persisted GR variables
mysql -uroot -e "SELECT variable_name, variable_value FROM performance_schema.persisted_variables WHERE variable_name LIKE 'group_replication%' ORDER BY variable_name;"

# Check running systemd services
sudo systemctl status mysql erawan-mysql-primary-check erawan-gr-watchdog.timer erawan-mysql-boot-recovery

# Check GR watchdog logs (last 50 lines)
sudo journalctl -t erawan-gr-watchdog -n 50

# Check boot recovery logs
sudo journalctl -u erawan-mysql-boot-recovery -n 50

# Check the primary-check endpoint agrees with GR state
curl -i http://127.0.0.1:9200/

# Show cluster status from mysqlsh (run on primary)
mysqlsh --no-wizard -e "shell.connect('clusteradmin@127.0.0.1:3306'); dba.get_cluster().status();"
```

---

## Cluster Architecture

```
            Application Clients
                    │
                    ▼
          ┌──────────────────────┐
          │       HAProxy        │
          │  :25041 (TCP)        │
          │  balance: first      │
          │  httpchk :9200 GET / │
          └────────┬─────────────┘
                   │  routes to whichever node's :9200 answers 200 (PRIMARY)
       ┌───────────┼───────────┐
       │           │           │
       ▼           ▼           ▼
  ┌─────────┐ ┌─────────┐ ┌─────────┐
  │ Node 1  │ │ Node 2  │ │ Node 3  │
  │ MySQL   │ │ MySQL   │ │ MySQL   │
  │ :3306   │ │ :3306   │ │ :3306   │
  │ check   │ │ check   │ │ check   │
  │ :9200   │ │ :9200   │ │ :9200   │
  └────┬────┘ └────┬────┘ └────┬────┘
       │            │           │
       └────────────┼───────────┘
                    │ Group Replication (InnoDB Cluster)
         ┌──────────┼──────────┐
         │          │          │
    ┌─────────┐ ┌─────────┐ ┌─────────┐
    │Primary  │ │Secondary│ │Secondary│
    │:3306    │ │:3306    │ │:3306    │
    │(writes) │ │(reads)  │ │(reads)  │
    └─────────┘ └─────────┘ └─────────┘
         └──────────┼──────────┘
                    │
            ┌───────────────┐
            │ InnoDB Cluster │
            │ GR consensus   │
            │ (built into    │
            │  MySQL engine) │
            └───────────────┘
```

### Failover flow

```
1. Primary node dies (or is gracefully switched over)
           │
           ▼
2. Group Replication detects loss / re-elects (≤30s for a crash)
           │
           ▼
3. Remaining nodes vote — majority elects new primary
           │
           ▼
4. Old primary's :9200 starts answering 503; new primary's :9200 answers 200
           │
           ▼
5. HAProxy's next health check (sub-second interval) marks the old primary
   down and the new primary up; new connections route to the new primary
   (existing connections from the dead/demoted node fail and reconnect)
```

---

## Metrics Collection

Metrics are collected from **Prometheus exporters** running on each node — no database credentials or direct SQL connections are needed:

```
POST /cluster/mysql/metrics
  { "job_id": "abc", "proxy_port": 25041 }
           │
           ▼
  Resolve node_ips from stored job
           │
           ▼
  Scrape mysqld_exporter :9104 on each node (parallel)
  Scrape node_exporter :9100 on each node (parallel)
           │
           ▼
  Discover primary from Group Replication member info in exporter data
           │
           ▼
  Aggregate per-category metrics, return JSON
```

`mysqld_exporter` and `node_exporter` must be running on every DB node. The API contacts exporters directly on node IPs — HAProxy is not in the metric path.

---

## Stop / Start

Stop and start the whole cluster without losing data (planned maintenance,
VM resizing, etc.).

```
POST /cluster/mysql/jobs/{jobID}/stop
  1. Disable GR autostart on all nodes         (group_replication_start_on_boot
                                                = OFF, so the next start boots
                                                clean instead of racing a half
                                                -formed rejoin)
  2. systemctl stop mysql on all secondaries   (primary keeps serving)
  3. systemctl stop mysql on the primary       (clean InnoDB shutdown,
                                                redo log flushed)

POST /cluster/mysql/jobs/{jobID}/start        (alias: /recover)
  1. systemctl start mysql on all nodes
  2. Pause the GR watchdog on all nodes         (it would otherwise fight the
                                                reboot for GR state — both try
                                                to STOP/START GROUP_REPLICATION
                                                on the same node at once)
  3. Stop any half-started GR (nodes booted with autostart still ON from
     before this fix) — retried while MySQL error 3663 ("another
     START/STOP GROUP_REPLICATION is executing") clears, up to 60s
  4. dba.rebootClusterFromCompleteOutage() once, on the primary — the
     member with the most complete GTID set becomes primary; retried on
     3663 the same way, up to 90s
  5. Resume the GR watchdog and restart mysqld_exporter on all nodes —
     runs even if step 3/4 failed, so the cluster can still self-heal and
     metrics don't silently go dark
  6. Verify all members reached ONLINE — polled every 2s, job fails if
     they don't converge within the retry window
```

See [assets/diagram/mysql-cluster-flow.svg](assets/diagram/mysql-cluster-flow.svg) for the full step-by-step flow including deploy.

Data directories are never touched by either operation. Stop is rejected
while a deploy or member operation is running on the same cluster. The
same VMs/disks must come back for start — start does not rebuild
destroyed nodes (use add-member for that).

**Requires MySQL Shell ≥ 8.0.30** — checked at deploy-time preflight. Start's
forced-reboot path (`reboot_cluster_from_complete_outage(..., {"force": true})`)
uses the `force` option, which older shells don't support; catching this at
deploy avoids a working deploy that then fails on its first post-outage
recovery.

---

## Rollback

`POST /cluster/mysql/jobs/{jobID}/rollback` runs the rollback playbook, which:

1. Stops and disables the primary-check service and removes its script/unit
2. Dissolves the InnoDB Cluster via `mysqlsh`
3. Resets Group Replication state on every node so they stop trying to rejoin
   a cluster that no longer exists

---

## Important Behaviors

- `assume_prepared: true` — skips preflight and `dba.configureInstance()`. Use when nodes were already prepared in a prior run.
- Single-node clusters support rollback but not automatic failover.
- A 3-node cluster survives loss of 1 node and keeps quorum. A 2-node cluster cannot tolerate any node loss (no quorum majority).
- Multiple `member_ips` in one add-member request join sequentially, never in parallel; each successful node is recorded before the next join starts, and the job stops at the first failure.
- Stop/start (and recover) never touch data directories; a stopped cluster restarts from its existing on-disk state via `dba.rebootClusterFromCompleteOutage()`.
- The boot-recovery service on each node automatically recovers a complete 3-node outage. Node 1 bootstraps the cluster (`dba.reboot_cluster_from_complete_outage`); nodes 2 and 3 detect the cluster is already active and rejoin via `cluster.rejoinInstance()`. The GR watchdog (every 60s) is a second-chance safety net — but is paused for the duration of a `start`/`recover` job so it doesn't race the reboot.
- `mysqld_exporter` is restarted at the end of every `start`/`recover` job. It isn't tied to the `mysql` systemd unit, so a `stop` silences it along with the database; without this it would keep reporting "unreachable" after the next start even though MySQL itself is healthy again.
- Ansible connections use pipelining and a pinned Python interpreter, so a step no longer pays interpreter-discovery and per-module SSH round-trips on every host — this matters most when a node's sshd is slow under load (e.g. mid-clone).
