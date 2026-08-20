# erawan-cluster

**Version 1.02** — Cluster Management API for automated database cluster lifecycle management, live metrics collection, and HAProxy configuration.

---

## Tech Stack

| Component | Technology | Purpose |
|-----------|------------|---------|
| Language | Go 1.25+ | API server, job orchestration, metrics collection |
| HTTP Router | [go-chi/chi](https://github.com/go-chi/chi) | Request routing and middleware |
| Build | Makefile | Build, test, and run targets |
| Automation | Ansible | Cluster deploy, configuration, and member management via SSH |
| Proxy / Load Balancer | HAProxy | TCP proxy and SQL routing to DB cluster nodes |
| MySQL Cluster | MySQL InnoDB Cluster + MySQL Shell + primary-check endpoint | HA MySQL with Group Replication and automatic failover |
| PostgreSQL Cluster | PostgreSQL + Patroni + etcd | HA PostgreSQL with leader election and synchronous replication |

---

## Features

- **MySQL cluster lifecycle** — deploy, resume, rollback, stop/start (data-preserving), post-outage recovery, add/remove members, user and database management
- **PostgreSQL cluster lifecycle** — deploy, resume, stop/start (data-preserving), post-outage recovery, add/remove members, user and database management
- **Live metrics** — 8 MySQL categories and 9 PostgreSQL categories collected via Prometheus exporters (mysqld_exporter, postgres_exporter, node_exporter)
- **HAProxy management** — tenant config generation, member addition, hot reload (no restart)
- **Job-based async execution** — all cluster operations run as tracked background jobs with step-level progress
- **Encryption** — optional AES-256-GCM request/response body encryption

### Cluster lifecycle endpoints (per engine: `mysql` / `pgsql`)

| Endpoint | Purpose |
|----------|---------|
| `POST /cluster/{engine}/deploy` | Deploy a new cluster |
| `POST /cluster/{engine}/jobs/{id}/resume` | Resume a failed deploy from the last completed step |
| `POST /cluster/{engine}/stop` | Graceful, data-preserving shutdown (secondaries/standbys first, primary last); body: `{"job_id"}` |
| `POST /cluster/{engine}/start` | Start a stopped cluster from existing data (alias: `/jobs/{id}/recover`); body: `{"job_id"}` |
| `POST /cluster/mysql/jobs/{id}/rollback` | Dissolve a MySQL cluster (MySQL only) |
| `POST /cluster/{engine}/members` | Add members (joined one at a time) |
| `DELETE /cluster/{engine}/members` | Remove a member |
| `DELETE /cluster/pgsql/dcs` | Release the cluster's namespace on the shared control-plane etcd when decommissioning it (PostgreSQL, `SHARED_CONTROL_PLANE` deployments only); body: `{"job_id"}` |
| `POST /cluster/{engine}/metrics` | Live metrics via Prometheus exporters |

See [doc/api.md](doc/api.md) for the full API reference.  
See [doc/proxy-architecture.md](doc/proxy-architecture.md) for the system design.  
See [doc/mysql.md](doc/mysql.md) and [doc/pgsql.md](doc/pgsql.md) for cluster detail.  
See [doc/control-plane.md](doc/control-plane.md) to stand up the shared control-plane etcd (`SHARED_CONTROL_PLANE`).  
See [doc/security.md](doc/security.md) for the security posture and hardening knobs.  
See [doc/adding-an-engine.md](doc/adding-an-engine.md) to add a new database engine.

---

## Requirements

### API (proxy) host
- Go 1.25+
- `ansible-playbook` installed
- SSH client available
- SSH access to all target DB nodes
- HAProxy installed

### MySQL target nodes
- MySQL installed and running
- `mysqlsh` (MySQL Shell) installed
- Local MySQL administration as OS `root` via Unix socket
- Nodes reachable from the proxy via SSH
- Nodes reachable from each other on MySQL port (default 3306)

### PostgreSQL target nodes
- PostgreSQL installed on all nodes
- `patroni[etcd]` installed
- `etcd` installed
- Ports 2379, 2380, 5432, 8008 reachable between nodes

---

## Quick Start

Recommended setup for a fresh Ubuntu 24.04 proxy node.

### 1. Install system packages
```bash
sudo apt update
sudo apt install -y git curl make
# Install Go 1.25+ from https://go.dev/dl/ (apt golang-go may be too old)
```

### 2. Clone and build
```bash
git clone https://github.com/Zii-Factorial/erawan-cluster.git
cd erawan-cluster
make tidy
make build
```

### 3. Run the Ubuntu installer
```bash
sudo bash scripts/install-ubuntu.sh
```

Installs: `haproxy`, `ansible`, `openssh-client`, the API binary at `/usr/local/bin/erawan-cluster`, playbooks at `/opt/erawan-cluster/cluster`, and the systemd service `erawan-cluster`.

### 4. Generate the SSH key on the proxy node
```bash
sudo install -d -o erawan -g erawan -m 0700 /var/lib/erawan-cluster/keys
sudo -u erawan ssh-keygen -t rsa -b 4096 -N '' -C 'clusterops@proxy-node' \
  -f /var/lib/erawan-cluster/keys/clusterops_rsa
sudo cat /var/lib/erawan-cluster/keys/clusterops_rsa.pub
```

### 5. Trust the public key on every DB node
```bash
# On each DB node:
sudo useradd -m -s /bin/bash clusterops || true
sudo install -d -o clusterops -g clusterops -m 700 /home/clusterops/.ssh
echo 'PASTE_PROXY_PUBLIC_KEY_HERE' | sudo tee -a /home/clusterops/.ssh/authorized_keys
sudo chown clusterops:clusterops /home/clusterops/.ssh/authorized_keys
sudo chmod 600 /home/clusterops/.ssh/authorized_keys
echo 'clusterops ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/clusterops
sudo chmod 440 /etc/sudoers.d/clusterops
```

### 6. Configure the API service
```bash
sudo nano /etc/erawan-cluster/.env
```

Minimum required:
```env
ENV=prod
API_HOST=127.0.0.1
API_PORT=8080
API_KEY=CHANGE_THIS_TO_A_LONG_RANDOM_SECRET
TENANTS_DIR=/var/lib/erawan-cluster/haproxy/tenants
HAPROXY_RELOAD_CMD=sudo /bin/systemctl reload haproxy
CLUSTER_STATE_DIR=/var/lib/erawan-cluster/cluster/jobs
# Optional: when set, job state is stored in PostgreSQL instead of files.
# Existing jobs under CLUSTER_STATE_DIR are migrated into the DB at startup.
# DB_CONNECTION=postgres://user:pass@127.0.0.1:5432/erawan?sslmode=disable
MYSQL_DEPLOY_PLAYBOOK=/opt/erawan-cluster/cluster/mysql/playbooks/deploy.yml
MYSQL_ROLLBACK_PLAYBOOK=/opt/erawan-cluster/cluster/mysql/playbooks/rollback.yml
PGSQL_DEPLOY_PLAYBOOK=/opt/erawan-cluster/cluster/pgsql/playbooks/deploy.yml
CLUSTER_SSH_USER=clusterops
CLUSTER_SSH_PRIVATE_KEY_PATH=/var/lib/erawan-cluster/keys/clusterops_rsa
```

### 7. Start and verify
```bash
sudo systemctl daemon-reload
sudo systemctl reload haproxy || sudo systemctl start haproxy
sudo systemctl restart erawan-cluster
sudo systemctl status erawan-cluster --no-pager
curl http://127.0.0.1:8080/health
```

### 8. Test SSH connectivity
```bash
sudo -u erawan ssh -i /var/lib/erawan-cluster/keys/clusterops_rsa clusterops@<db-ip> 'whoami'
sudo -u erawan ssh -i /var/lib/erawan-cluster/keys/clusterops_rsa clusterops@<db-ip> 'sudo -n whoami'
```

Both should succeed (`clusterops`, then `root`).

---

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `API_ADDR` | `:8080` | Listen address (overrides API_HOST + API_PORT) |
| `API_HOST` | `0.0.0.0` | Bind host |
| `API_PORT` | `8080` | Bind port |
| `API_KEY` | — | Required API key sent in `X-API-Key` header |
| `ENCRYPTION_KEY` | — | Optional AES-256-GCM key for body encryption |
| `DB_CONNECTION` | — | Optional PostgreSQL connection string for job storage; when unset, jobs stay in `CLUSTER_STATE_DIR` |
| `PROXY_HOST` | `127.0.0.1` | HAProxy address used for metric SQL connections |
| `TENANTS_DIR` | `/var/lib/erawan-cluster/haproxy/tenants` | HAProxy tenant config directory |
| `HAPROXY_RELOAD_CMD` | `sudo /bin/systemctl reload haproxy` | HAProxy reload command |
| `HAPROXY_RELOAD_TIMEOUT_SECONDS` | `15` | Timeout for reload command |
| `HAPROXY_MAIN_CONFIGS` | — | Comma-separated extra HAProxy config files to include |
| `CLUSTER_STATE_DIR` | `/var/lib/erawan-cluster/cluster/jobs` | Root job state directory |
| `MYSQL_CLUSTER_STATE_DIR` | `<CLUSTER_STATE_DIR>/mysql` | MySQL job state directory |
| `PGSQL_CLUSTER_STATE_DIR` | `<CLUSTER_STATE_DIR>/pgsql` | PostgreSQL job state directory |
| `ANSIBLE_PLAYBOOK_BIN` | `ansible-playbook` | Ansible binary path |
| `CLUSTER_SSH_USER` | — | Required SSH login user for all cluster jobs |
| `CLUSTER_SSH_PRIVATE_KEY_PATH` | — | Required SSH private key path |
| `MYSQL_DEPLOY_PLAYBOOK` | `<project>/cluster/mysql/playbooks/deploy.yml` | MySQL deploy playbook |
| `MYSQL_ROLLBACK_PLAYBOOK` | `<project>/cluster/mysql/playbooks/rollback.yml` | MySQL rollback playbook |
| `MYSQL_ADD_MEMBER_PLAYBOOK` | `<project>/cluster/mysql/playbooks/add_member.yml` | MySQL add-member playbook |
| `MYSQL_REMOVE_MEMBER_PLAYBOOK` | `<project>/cluster/mysql/playbooks/remove_member.yml` | MySQL remove-member playbook |
| `PGSQL_DEPLOY_PLAYBOOK` | `<project>/cluster/pgsql/playbooks/deploy.yml` | PostgreSQL deploy playbook |
| `PGSQL_ADD_MEMBER_PLAYBOOK` | `<project>/cluster/pgsql/playbooks/add_member.yml` | PostgreSQL add-member playbook |
| `PGSQL_REMOVE_MEMBER_PLAYBOOK` | `<project>/cluster/pgsql/playbooks/remove_member.yml` | PostgreSQL remove-member playbook |
| `MYSQL_STOP_PLAYBOOK` | `<project>/cluster/mysql/playbooks/stop.yml` | MySQL cluster stop playbook |
| `PGSQL_STOP_PLAYBOOK` | `<project>/cluster/pgsql/playbooks/stop.yml` | PostgreSQL cluster stop playbook |
| `MYSQL_ANSIBLE_DEBUG` | `false` | Stream live Ansible logs to journal |
| `MYSQL_ANSIBLE_VERBOSITY` | `0` | Ansible verbosity level (1–4) |
| `PGSQL_ANSIBLE_DEBUG` | `false` | Stream live PostgreSQL Ansible logs to journal |
| `PGSQL_ANSIBLE_VERBOSITY` | `0` | PostgreSQL Ansible verbosity level (1–4) |
| `CLUSTER_ANSIBLE_DEBUG` | `false` | Debug flag for both engines |
| `CLUSTER_ANSIBLE_VERBOSITY` | `0` | Verbosity for both engines |
| `CLUSTER_STEP_OUTPUT_MAX_CHARS` | `8000` | Max chars captured per Ansible step (per-engine: `MYSQL_STEP_OUTPUT_MAX_CHARS`, `PGSQL_STEP_OUTPUT_MAX_CHARS`); auto-raised to 200000 when debug is on |
| `CLUSTER_SSH_INSECURE_HOST_KEY` | `false` | Disable SSH host-key verification (greenfield bootstrap only; leave `false` in production) |
| `CLUSTER_SSH_KNOWN_HOSTS` | — | Path to a `known_hosts` file to pin node host keys |
| `CLUSTER_DB_TLS_MODE` | `verify` | MySQL admin TLS mode: `verify` (default) \| `skip-verify` \| `false` (disabled) |
| `CLUSTER_DB_SSL_MODE` | `verify-full` | PostgreSQL admin SSL mode: `verify-full` (default) \| `verify-ca` \| `require` \| `disable` |
| `CLUSTER_MAX_CONCURRENT_JOBS` | `4` | Cap on simultaneous background cluster jobs |
| `SHARED_CONTROL_PLANE` | — | Private IP of the shared control-plane etcd. Set it and new PostgreSQL clusters keep no etcd on their own nodes (any node count is supported); unset keeps the classic per-node etcd quorum. Existing clusters are never migrated |
| `CONTROL_PLANE_ETCD_ROOT_PASSWORD` | — | etcd root password used to manage per-tenant RBAC. Required when `SHARED_CONTROL_PLANE` is set — the process refuses to start without it |
| `CONTROL_PLANE_ETCD_ROOT_USER` | `root` | etcd superuser on the control plane |
| `CONTROL_PLANE_ETCD_CLIENT_PORT` | `2379` | Control-plane etcd client port |
| `CONTROL_PLANE_ETCD_CACERT` / `_CERT` / `_KEY` | `/etc/etcd/ssl/{ca,cp-etcd-01,cp-etcd-01-key}.pem` | TLS material as it exists **on the control plane**; only the CA is copied to DB nodes |
| `CONTROL_PLANE_DCS_NAMESPACE` | `/db/patroni/` | Key namespace; a tenant owns `<namespace><cluster_name>/` |
| `CONTROL_PLANE_DCS_TENANT_PREFIX` | `patroni` | Names the per-tenant etcd role/user (`<prefix>-<cluster>-role`/`-user`) |
| `CONTROL_PLANE_ETCD_CLIENT_CERT` | `true` | Issue each cluster its own etcd client certificate. Required by a control plane running `client-cert-auth: true`, which refuses the TLS handshake without one. Needs the CA key below |
| `CONTROL_PLANE_ETCD_CA_KEY` | `/etc/etcd/ssl/ca-key.pem` | CA private key on the control plane that signs tenant client certificates. [control-plane.md](doc/control-plane.md) keeps the CA in `/root/pki/etcd-ca` so the etcd service account cannot read it — on a host built that way this must be set explicitly |
| `CONTROL_PLANE_ETCD_CLIENT_CERT_DIR` | `/etc/etcd/ssl/erawan-clients` | Where minted tenant certificates are kept on the control plane |
| `CONTROL_PLANE_ETCD_CLIENT_CERT_DAYS` | `3650` | Validity of a minted tenant certificate |
| `CONTROL_PLANE_ETCD_NODE_CERT_PATH` / `_KEY_PATH` | `/etc/patroni/etcd-client{,-key}.pem` | Where the tenant certificate lands on each DB node |
| `CONTROL_PLANE_ETCD_NODE_CA_PATH` | `/etc/patroni/etcd-ca.pem` | Where the control plane's CA lands on each DB node |
| `CONTROL_PLANE_ETCD_NODE_CA_OWNER` | `postgres` | OS user that must read that CA (the engine's service account) |
| `CONTROL_PLANE_SSH_USER` / `_PRIVATE_KEY_PATH` / `_PORT` | — | Overrides for reaching the control plane; defaults to the cluster SSH credentials |
| `CONTROL_PLANE_DCS_PROVISION_PLAYBOOK` | `<project>/cluster/shared/playbooks/control_plane_dcs_provision.yml` | Shared tenant-provisioning playbook |
| `CONTROL_PLANE_DCS_CLEANUP_PLAYBOOK` | `<project>/cluster/shared/playbooks/control_plane_dcs_cleanup.yml` | Shared tenant-release playbook |
| `ENABLE_PPROF` | `false` | Expose `net/http/pprof` on `127.0.0.1:6060` (debugging only, no auth) |

---

## Make Commands

```bash
make tidy       # go mod tidy
make fmt        # format source
make build      # build binary to ./bin
make run        # run API directly
make test       # run all tests
make test-unit  # run only tests/unit/
make cover      # tests with coverage attributed to production packages
make check      # full quality gate: fmtcheck + vet + staticcheck + vulncheck + test
make vulncheck  # scan for known Go CVEs (govulncheck)
```

### Database migrations

`make migration` requires `DB_CONNECTION` to be set (a PostgreSQL URL, e.g.
`postgres://user:pass@127.0.0.1:5432/erawan?sslmode=disable`). It's read via
`-include .envrc` / `-include /etc/erawan-cluster/.env` at the top of the
Makefile — export it in one of those, or inline it. This applies to every
subcommand below, including `TABLE=` scaffolding, which never touches the DB
but is still gated by the same check in `scripts/migrate.sh`.

```bash
make migration                  # apply all pending migrations
make migration TABLE=<name>     # scaffold a new NNN_<name>.up.sql / .down.sql pair
make migration ROLLBACK=<name>  # run the matching .down.sql
```

Applied versions are tracked in the `erawan_schema_migrations` table, created
automatically on first run.

---

## Code Structure

```
cmd/api/
  main.go          entry point, service wiring
  api.go           application struct, route registration
  config.go        environment config loading (all tunables)
  setup.go         service builders (MySQL, PostgreSQL, HAProxy)
  health.go        health check handler
  version.go       version constant
  mysql/
    handler.go     MySQL cluster + DB manager HTTP handlers
  pgsql/
    handler.go     PostgreSQL cluster + DB manager HTTP handlers
  haproxy/
    handler.go     HAProxy management HTTP handlers

internal/
  cluster/core/    Shared job state, Ansible runner, concurrency, metric types
  cluster/mysql/   MySQL cluster service + Prometheus exporter metrics
  cluster/pgsql/   PostgreSQL cluster service + Prometheus exporter metrics
  haproxy/         HAProxy config service (listen blocks, backup/rollback)
  render/          Shared JSON response helpers
  security/        API key middleware, AES-GCM cipher
  env/             Environment variable helpers
```

---

## Security

- All requests require `X-API-Key` header
- Request body capped at 2 MiB
- Unknown JSON fields rejected
- IP, port, and username input validated
- Job state files stored with restrictive permissions (`0700` dir, `0600` files)
- User input never shell-interpolated into Ansible commands
- Optional AES-256-GCM body encryption via `ENCRYPTION_KEY`
- SQL metric connections always route through HAProxy (`PROXY_HOST`), never directly to DB VM IPs
