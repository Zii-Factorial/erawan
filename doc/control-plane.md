# Standing up the shared control plane

The shared control plane is one etcd that every tenant's Patroni borrows its
quorum from, so a PostgreSQL cluster of any size — one node included — needs no
etcd of its own. What a tenant is issued on it, and why the design looks like
this, is in [pgsql.md → Shared control-plane DCS](pgsql.md#shared-control-plane-dcs)
and [cluster/shared/README.md](../cluster/shared/README.md).

Erawan **consumes** that host and never provisions it. It creates and deletes
per-tenant roles, users, key prefixes, client certificates and firewall grants —
nothing else. Standing up etcd, its TLS material, the root user and `auth
enable` is this document.

Steps 1–9 below build one, in order. After them come the operational notes:
[rollback and revoking access](#rollback-and-revoking-access),
[more members](#adding-more-control-plane-members),
[changing its IP](#changing-the-control-planes-private-ip), [rotation](#rotation)
and [troubleshooting](#troubleshooting).

**Run every command as root** (`sudo -i`). The files live in
`/root/pki/etcd-ca` and `/etc/etcd/ssl`, neither of which a normal user can
write, so a half-`sudo`'d paste fails partway through.

## What you end up with

| Piece | Value used throughout this guide |
|---|---|
| Host / etcd member name | `cp-etcd-01` |
| Private IP | `10.10.3.66` |
| Client port (DB nodes, erawan) | `2379` |
| Peer port (other control-plane members only) | `2380` |
| CA — signs everything, never leaves this host | `/root/pki/etcd-ca/{ca.pem,ca-key.pem,ca-config.json}` |
| CA certificate etcd and the nodes trust | `/etc/etcd/ssl/ca.pem` — a copy of the one above |
| Server certificate | `/etc/etcd/ssl/cp-etcd-01.pem` + `-key.pem` |
| etcd config | `/etc/etcd/etcd.conf.yml` |
| Tenant certificates (minted by erawan) | `/etc/etcd/ssl/erawan-clients/<engine>/` |
| Tenant keys | `/db/patroni/<cluster>/` |

The CA sits outside `/etc/etcd/ssl` deliberately. etcd never reads the CA key —
only erawan does, over SSH, as root — so keeping it out of the directory the
etcd service account owns means compromising that account does not compromise
the CA or any credential ever issued from it. `/etc/etcd/ssl` ends up holding
exactly what etcd itself opens: its own key pair and the public CA certificate.

That choice has one consequence, and it is the one thing in this document that
bites after everything looks fine: erawan looks for the CA key at
`/etc/etcd/ssl/ca-key.pem` unless told otherwise, so `CONTROL_PLANE_ETCD_CA_KEY`
becomes a required setting rather than an optional one (step 9). Left unset, the
first deploy creates the cluster's etcd user and role, then fails with
`/etc/etcd/ssl/ca-key.pem does not exist on the control plane, so a client
certificate cannot be signed`.

The member name is not cosmetic: it names the server certificate, and erawan
reads that certificate by path. `CONTROL_PLANE_ETCD_CERT` / `_KEY` default to
`cp-etcd-01{,-key}.pem`, so either the host is called `cp-etcd-01` or you set
the name on both sides.

The IP is an example throughout, not a constant: this host is a template, and a
clone of it comes up on whatever address it is handed. Re-addressing one —
cloned or live — is [changing the control plane's private
IP](#changing-the-control-planes-private-ip).

## Prerequisites

- **A VM with etcd 3.4.** Ubuntu 24.04 packages `etcd-server`
  `3.4.30-1ubuntu0.24.04.3` — the version the four requirements in
  [pgsql.md](pgsql.md#configuring-the-control-plane) were verified against.
  Older Ubuntu LTS releases ship the 3.3 series, which nothing here is tested
  against.
- **Network.** DB nodes reach `2379`; erawan opens that port per tenant with UFW
  during provisioning, scoped to the tenant's node addresses. `2380` is needed
  only between control-plane members. Nothing needs to be public.
- **SSH from the erawan host.** The provisioning playbooks run with
  `become: true`, so they operate as root on this host: they run `etcdctl` and
  sign tenant certificates *here*, and the CA key never leaves the machine.
  Credentials default to the cluster's own SSH key;
  `CONTROL_PLANE_SSH_USER` / `_PRIVATE_KEY_PATH` / `_PORT` override.
- **Decide the failure domain first.** Patroni's `ttl` is 30s, so a control
  plane down longer than that costs **every** tenant its leader lock and demotes
  every primary to read-only until it returns. One member is fine for a lab and
  is a shared single point of failure in production; see
  [more members](#adding-more-control-plane-members) for what erawan does and
  does not do with more than one.

## Step 1 — Install etcd and the certificate tooling

```bash
apt update
apt install -y etcd-server etcd-client golang-cfssl gettext-base

# The package auto-starts etcd with its own defaults, which are not the ones
# below. Stop it before it writes any state.
systemctl stop etcd

# Two directories: the CA's, which is root's alone, and etcd's, which holds
# only what etcd reads. Everything in steps 2 and 3 is signed in the first.
mkdir -p /etc/etcd/ssl /var/lib/etcd /root/pki/etcd-ca
chmod 700 /root/pki/etcd-ca
cd /root/pki/etcd-ca
```

`golang-cfssl` provides `cfssl` and `cfssljson`, which sign everything in steps
2 and 3. `gettext-base` is not used by any step here — it is in the line because
the node image installs it anyway, and it does no harm.

## Step 2 — Create the CA

Once, on the first control plane. Everything on every control plane — server
certificates and every tenant certificate erawan ever mints — is signed by this
one CA, so a second control-plane host gets a *copy* of these three files, never
its own CA.

The signing profile first. It is reusable for every certificate you sign later:

```bash
cat > ca-config.json <<'EOF'
{
  "signing": {
    "default": {"expiry": "87600h"},
    "profiles": {
      "server": {
        "expiry": "87600h",
        "usages": ["signing","key encipherment","server auth","client auth"]
      }
    }
  }
}
EOF
```

`"client auth"` in the **server** profile is the load-bearing part and the
easiest thing to leave out. Under `client-cert-auth: true` the gRPC JSON gateway
dials etcd's own gRPC listener *as a client*, presenting this same certificate.
Without `clientAuth` every `/v3` request answers HTTP 503
`error reading server preface: remote error: tls: bad certificate`, while
`etcdctl` and `/version` keep working — see
[pgsql.md requirement 3](pgsql.md#3-the-control-planes-server-certificate-needs-clientauth).

Then the CA identity, and the CA itself:

```bash
cat > ca-csr.json <<'EOF'
{
  "CN": "etcd-ca",
  "key": {"algo": "rsa", "size": 2048},
  "ca": {"expiry": "175200h"}
}
EOF

# ca.pem = public (copied out in step 3), ca-key.pem = private (never leaves
# this directory)
cfssl gencert -initca ca-csr.json | cfssljson -bare ca
```

The `ca.expiry` line is worth keeping. Left out, cfssl gives the CA its own
default lifetime (5 years) while the profile above signs leaf certificates for
10 — and erawan mints tenant certificates for 3650 days by default. A CA that
expires before the certificates it signed takes every tenant down on that date,
so check what yours actually got, especially on a CA created earlier:

```bash
openssl x509 -in ca.pem -noout -subject -dates
```

| File | Where it lives | Who needs it |
|---|---|---|
| `ca.pem` | Here, plus a copy at `/etc/etcd/ssl/ca.pem` | etcd, erawan, and every DB node (installed as `/etc/patroni/etcd-ca.pem`) |
| `ca-key.pem` | `/root/pki/etcd-ca` only | Erawan alone. It signs tenant certificates with the key **in place, over SSH** — the key is never copied to a node, to the erawan host, or into `/etc/etcd/ssl` |
| `ca-config.json` | `/root/pki/etcd-ca` only | Whoever signs a server certificate — step 3, and re-addressing later |

## Step 3 — Sign the server certificate

Change `CN` and `hosts` to the real node. The IP is not optional: Patroni
verifies the server certificate against **the IP it dials**, and the node-side
check erawan runs fails by name on a missing SAN rather than letting the cluster
start and fail every DCS call later. `127.0.0.1` matters for the same reason —
it is the address the gateway itself dials.

```bash
cd /root/pki/etcd-ca

cat > cp-etcd-01-csr.json <<'EOF'
{
  "CN": "cp-etcd-01",
  "hosts": ["cp-etcd-01", "10.10.3.66", "127.0.0.1", "localhost"],
  "key": {"algo": "rsa", "size": 2048}
}
EOF

cfssl gencert -ca=ca.pem -ca-key=ca-key.pem \
  -config=ca-config.json -profile=server \
  cp-etcd-01-csr.json | cfssljson -bare cp-etcd-01

# Everything etcd reads, and nothing more. The CA key and the signing profile
# stay behind in /root/pki/etcd-ca.
cp ca.pem cp-etcd-01.pem cp-etcd-01-key.pem /etc/etcd/ssl/

# Kept here: ca.pem, ca-key.pem, ca-config.json, cp-etcd-01.pem, cp-etcd-01-key.pem
rm -f *.csr *-csr.json
```

## Step 4 — Set the file permissions

```bash
# etcd's directory: the public CA certificate and this host's own key pair.
chown etcd:etcd /etc/etcd/ssl/*
chmod 644 /etc/etcd/ssl/*.pem
chmod 600 /etc/etcd/ssl/*-key.pem     # after the 644 — key files match *.pem too

# The CA's directory: root's alone, and not traversable by anyone else.
chown -R root:root /root/pki/etcd-ca
chmod 700 /root/pki/etcd-ca
chmod 600 /root/pki/etcd-ca/ca-key.pem
```

One more tightening, once erawan has minted tenant certificates on this host —
they are live credentials for other people's clusters:

```bash
chown -R root:root /etc/etcd/ssl/erawan-clients && chmod 700 /etc/etcd/ssl/erawan-clients
```

Keeping the CA out of `/etc/etcd/ssl` is what makes that first wildcard `chown`
safe to re-run: it can no longer hand the CA key to the etcd service account,
because the key is not in the directory being swept. It can still re-take
`erawan-clients`, so once tenants exist here, set ownership per file rather than
sweeping the directory.

Erawan writes two things of its own into `/etc/etcd/ssl`, both as root, and
neither needs creating by hand: the minted pairs under `erawan-clients/<engine>/`
and the serial counter `ca.srl` it keeps for them. That counter is erawan's
alone — cfssl gives every certificate it signs a random serial and keeps no
counter — so the two never have to be reconciled.

## Step 5 — Write `/etc/etcd/etcd.conf.yml`

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

# Load-bearing, and NOT the default here — see below.
enable-grpc-gateway: true

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

**`enable-grpc-gateway: true` is the single most missable line in this
document.** Patroni speaks etcd v3 over HTTP (`POST /v3/...`), and those paths
exist only when the gateway is on. etcd defaults it to `true` for command-line
flags but **`false`** under `--config-file`, which is how this host starts — so
a config file that merely omits the key serves no `/v3` at all. Nothing warns
you: `/version` answers, `etcdctl` works (it speaks gRPC on the same port), and
erawan provisions the tenant successfully. Then every Patroni call gets a
plain-text `404 page not found`, which Patroni JSON-decodes into the integer
`404` and dies on `AttributeError: 'int' object has no attribute 'get'`.

The loopback entry in `listen-client-urls` is what the gateway dials when it
proxies an inbound request to etcd's own gRPC listener. Keep it.

## Step 6 — Start etcd from that config

The packaged unit starts etcd from `/etc/default/etcd` with flags and
environment. Point it at the config file instead — and clear the environment the
package sets, so the two cannot read as if both applied:

```bash
mkdir -p /etc/systemd/system/etcd.service.d

tee /etc/systemd/system/etcd.service.d/override.conf > /dev/null <<'EOF'
[Service]
Environment=
ExecStart=
ExecStart=/usr/bin/etcd --config-file=/etc/etcd/etcd.conf.yml
EOF

systemctl stop etcd
# State from the package's own auto-start, under the name and defaults it used.
# Left in place it collides with the member this config bootstraps.
rm -rf /var/lib/etcd/default
systemctl daemon-reload
systemctl enable --now etcd
systemctl status etcd
```

`rm -rf /var/lib/etcd/default` is safe **only** because that directory is the
package's, never this member's: the config above keeps its state in
`/var/lib/etcd/member`. Once the control plane is live, never clear that one to
fix a startup error — it holds every tenant's Patroni state, every etcd user and
role, and the `auth enable` flag.

## Step 7 — Verify the certificate and the gateway

```bash
openssl x509 -in /etc/etcd/ssl/cp-etcd-01.pem -noout -ext extendedKeyUsage -ext subjectAltName
```

Both lines must be there:

```
X509v3 Extended Key Usage:
    TLS Web Server Authentication, TLS Web Client Authentication
X509v3 Subject Alternative Name:
    DNS:cp-etcd-01, DNS:localhost, IP Address:10.10.3.66, IP Address:127.0.0.1
```

A missing `TLS Web Client Authentication` means `ca-config.json` had no
`"client auth"` in the server profile. Fixing the profile changes nothing
already issued — re-sign (step 3) and restart etcd.

Then the gateway. `curl https://10.10.3.66:2379/version` is **not** the test: it
is answered by etcd's own HTTP handler and stays green even when the API Patroni
uses is entirely unavailable. Use a `/v3` path:

```bash
curl -s -o /dev/null -w '%{http_code}\n' \
  --cacert /etc/etcd/ssl/ca.pem \
  --cert /etc/etcd/ssl/cp-etcd-01.pem \
  --key /etc/etcd/ssl/cp-etcd-01-key.pem \
  -X POST https://10.10.3.66:2379/v3/auth/authenticate -d '{}'
```

| Code | Meaning |
|---|---|
| `404` | Gateway off. Fix `enable-grpc-gateway` (step 5) and restart etcd |
| `400` | Gateway **live**. This is the CN guard firing on the server certificate's own CommonName, which is expected here — tenant certificates are minted without one |
| `503` | Server certificate has no `clientAuth` |

Erawan re-runs the equivalent check from every DB node on every deploy,
start/recover and add-member, and fails the job with the cause named — so this
is a pre-flight, not the only line of defence.

## Step 8 — Create the root user and enable RBAC

Prefix-scoped RBAC is the only thing keeping tenants off each other's keys.
Erawan creates per-tenant roles and users but never runs `auth enable`, on
purpose: turning authentication on for a host it does not own is not its call.

```bash
export ETCDCTL_API=3
export ETCDCTL_ENDPOINTS=https://10.10.3.66:2379
export ETCDCTL_CACERT=/etc/etcd/ssl/ca.pem
export ETCDCTL_CERT=/etc/etcd/ssl/cp-etcd-01.pem
export ETCDCTL_KEY=/etc/etcd/ssl/cp-etcd-01-key.pem

etcdctl endpoint health
etcdctl user add root --interactive=false --new-user-password='<root-pw>'
etcdctl auth enable
etcdctl --user "root:<root-pw>" user list
```

That password is what `CONTROL_PLANE_ETCD_ROOT_PASSWORD` must match. Leave auth
off and deploys still succeed — Patroni tolerates it, and the provisioning step
only warns — but every tenant can then read and write every other tenant's
Patroni state.

## Step 9 — Point erawan at it

On the erawan host (`.envrc` / the service environment):

```bash
SHARED_CONTROL_PLANE=10.10.3.66
CONTROL_PLANE_ETCD_ROOT_PASSWORD=<root-pw>     # start-up fails without it

# Required with the CA layout in this document. The built-in default is
# /etc/etcd/ssl/ca-key.pem, and step 3 deliberately did not put it there.
CONTROL_PLANE_ETCD_CA_KEY=/root/pki/etcd-ca/ca-key.pem

# Defaults shown; set only if you changed the names above
# CONTROL_PLANE_ETCD_CLIENT_PORT=2379
# CONTROL_PLANE_ETCD_CACERT=/etc/etcd/ssl/ca.pem
# CONTROL_PLANE_ETCD_CERT=/etc/etcd/ssl/cp-etcd-01.pem
# CONTROL_PLANE_ETCD_KEY=/etc/etcd/ssl/cp-etcd-01-key.pem
# CONTROL_PLANE_ETCD_CLIENT_CERT=true                     # leave on: step 5 set client-cert-auth
# CONTROL_PLANE_ETCD_CLIENT_CERT_DIR=/etc/etcd/ssl/erawan-clients
```

`CONTROL_PLANE_ETCD_CA_KEY` is read once at start-up, so it needs a restart of
the erawan service, not just an edit. Getting it wrong fails late and only
partly: the tenant's etcd role, user and key prefix are created first, the
signing step is what fails, and re-running after the fix converges the objects
already there rather than tripping over them.

Do **not** answer that failure with `CONTROL_PLANE_ETCD_CLIENT_CERT=false`. With
`client-cert-auth: true` from step 5, a node presenting no certificate has its
TLS handshake aborted — `tlsv13 alert certificate required` — and Patroni sits
on `waiting on etcd` forever. That switch is for a control plane that does not
require client certificates at all.

Restart erawan and deploy a PostgreSQL cluster. The `control_plane_dcs` step
creates the tenant's role, user and key prefix, mints its client certificate
into `/etc/etcd/ssl/erawan-clients/pgsql/`, installs the CA and that pair on
each node, opens `2379` to those nodes, and verifies from each node that it can
authenticate. The full variable list is in the
[README configuration table](../README.md#configuration).

Existing clusters are never migrated: each job records the DCS layout it was
deployed with, so setting this variable cannot re-point a cluster that is
already running its own etcd.

## Rollback and revoking access

Three different things get rolled back here, and they are not interchangeable:
a tenant's access, one node's access, and a change to the control plane itself.

### Revoking one tenant

```
DELETE /cluster/pgsql/dcs      body: {"job_id": "<job>"}
```

This runs `control_plane_dcs_cleanup.yml` against the control plane **alone**,
so it still works after the tenant's VMs are destroyed. For that cluster it
removes:

- every key under `/db/patroni/<cluster>/`,
- the etcd user `patroni-<cluster>-user` and role `patroni-<cluster>-role`,
- every certificate pair matching `<cluster>-user-*` in
  `/etc/etcd/ssl/erawan-clients/<engine>/`, and
- the UFW grants on `2379` for that cluster's node addresses.

Nothing else releases them. The objects are named after the cluster, so leaving
them behind accumulates state on shared infrastructure — and a later cluster of
the same name inherits a password and a certificate issued to machines that no
longer exist.

It takes effect immediately: the cluster loses its DCS, Patroni loses its leader
lock, and every node goes read-only. This is a decommissioning step, not a way
to pause a cluster.

By hand, if erawan cannot reach the host (same `ETCDCTL_*` environment as step 8):

```bash
etcdctl --user "root:<root-pw>" del --prefix /db/patroni/<cluster>/
etcdctl --user "root:<root-pw>" user delete patroni-<cluster>-user
etcdctl --user "root:<root-pw>" role delete patroni-<cluster>-role
rm -f /etc/etcd/ssl/erawan-clients/pgsql/<cluster>-user-*
ufw delete allow from <node-ip> to any port 2379 proto tcp
```

### Revoking one node

Removing a member passes the departing address to the provisioning run as
`dcs_revoke_ips`, which drops its UFW grant on `2379`. The tenant's user, role
and keys stay — the remaining nodes still need them — and a node rebuilt by a
scale operation is granted again on the next provisioning run, so neither needs
doing by hand.

What this does **not** do is invalidate the certificate that node still holds on
disk. It is the tenant's certificate, shared by every node of the cluster, so
withdrawing it means [rotating](#rotation) it for the whole tenant. Destroy the
VM, or rotate if the disk left your control.

### Rolling back a change to this host

etcd reads its config and certificates only at start, so rolling back a change
is: put the old file back, restart. That only works if the old file still
exists — and `cfssl … | cfssljson -bare cp-etcd-01` overwrites a certificate
pair in place, with no backup. Before editing or re-signing:

```bash
cp -a /etc/etcd/etcd.conf.yml /etc/etcd/etcd.conf.yml.$(date +%Y%m%dT%H%M%S)
cp -a /etc/etcd/ssl/cp-etcd-01.pem     /etc/etcd/ssl/cp-etcd-01.pem.bak
cp -a /etc/etcd/ssl/cp-etcd-01-key.pem /etc/etcd/ssl/cp-etcd-01-key.pem.bak
```

A bad certificate is then one `cp` and one `systemctl restart etcd` away from
being undone, instead of a re-sign under pressure with every tenant down.

Data is a separate rollback. Take a snapshot before anything that touches
membership or the data directory:

```bash
etcdctl --user "root:<root-pw>" snapshot save /var/backups/etcd-$(date +%F).db
```

Restoring is deliberate and offline — it rebuilds the data directory, and every
tenant's state goes back to the moment of the snapshot:

```bash
systemctl stop etcd
etcdctl snapshot restore /var/backups/etcd-2026-08-19.db   --name cp-etcd-01   --initial-cluster cp-etcd-01=https://10.10.3.66:2380   --initial-advertise-peer-urls https://10.10.3.66:2380   --data-dir /var/lib/etcd.restored
mv /var/lib/etcd /var/lib/etcd.broken
mv /var/lib/etcd.restored /var/lib/etcd
chown -R etcd:etcd /var/lib/etcd
systemctl start etcd
```

(On etcd 3.5 and later `snapshot restore` moved to `etcdutl`; on the 3.4 line
this guide targets it is still `etcdctl`.) Patroni rebuilds its own keys within
a few loop intervals, but RBAC comes back as the snapshot had it: a tenant
provisioned after the snapshot is simply missing, and heals on its next deploy,
start/recover or add-member. What is never a repair is deleting
`/var/lib/etcd/member` — that is not a rollback, it is the whole control plane.

### Turning the shared control plane off

Comment out `SHARED_CONTROL_PLANE` and `CONTROL_PLANE_ETCD_ROOT_PASSWORD` in
`/etc/erawan-cluster/.env` and restart the service, and **new** PostgreSQL
clusters go back to a per-node etcd quorum.

Do this only once no cluster is deployed against the control plane. A cluster's
stored spec records that its DCS is here, but the runner needs both that flag
*and* a configured control plane
([`usesControlPlane`](../internal/cluster/pgsql/runner.go)) — with the variable
gone, the next deploy or start/recover for such a cluster renders its
`patroni.yml` in classic mode, pointing Patroni at a local etcd its nodes do not
run. Decommission or redeploy those clusters first, then retire the host.

## Adding more control-plane members

Quorum on the control plane needs an odd number of members. Add them one at a
time, and register each with the running cluster **before** it boots:

```bash
# On an existing member:
etcdctl --user "root:<root-pw>" member add cp-etcd-02 \
  --peer-urls=https://10.10.3.67:2380
```

Build the new host through steps 1–6, copying `ca.pem`, `ca-key.pem` and
`ca-config.json` into its `/root/pki/etcd-ca` at step 2 rather than creating a
CA there, and give its `etcd.conf.yml` the whole membership and
`initial-cluster-state: existing` — a second member that bootstraps as `new`
forms its own one-member cluster instead of joining:

```yaml
name: cp-etcd-02
initial-cluster: cp-etcd-01=https://10.10.3.66:2380,cp-etcd-02=https://10.10.3.67:2380
initial-cluster-state: existing
initial-cluster-token: erawan-shared-cp
```

Two limits worth knowing before you build three of these:

- **Erawan talks to one address.** `SHARED_CONTROL_PLANE` is a single IP, and
  each node's Patroni config gets exactly that one endpoint with
  `use_proxies: true` (deliberate — a tenant must not discover and start dialling
  peers it does not own). Extra members make the etcd cluster survive losing
  one; they do not give the DB nodes a second address to fail over to unless
  that IP is a VIP you move.
- **Erawan's TLS material is per-host.** `CONTROL_PLANE_ETCD_CERT` names one
  certificate, so the host erawan SSHes into is the one whose name has to match.

## Changing the control plane's private IP

This host is a template. Clone the image and it boots on whatever address it is
handed, so the address in every file above is a value, not a constant — and the
cloud-init in
[`cluster/shared/cloudinit/etcd-control-plane.yml`](../cluster/shared/cloudinit/etcd-control-plane.yml)
is built around that: `etcd-regen-config.service` detects the private IP at
boot, re-signs the server certificate for it, re-renders `etcd.conf.yml` from
`/etc/etcd/etcd.conf.yml.j2` and restarts etcd.

**That image expects the CA in `/etc/etcd/ssl`, not in `/root/pki/etcd-ca`.**
Both `etcd-regen-config.service` and `etcd-make-ca.sh` look for `ca.pem`,
`ca-key.pem` and `ca-config.json` there, and the regen service exits with
`CA files ... not found` when they are absent — so a host built by hand from
this document does not get boot-time re-signing until the CA is put where the
image looks. Pick one and know which you are on: keep the CA in `/etc/etcd/ssl`
if you want that automation (and leave `CONTROL_PLANE_ETCD_CA_KEY` at its
default), or keep the layout in this document and re-sign by hand as below.
Erawan is indifferent either way — it reads `CONTROL_PLANE_ETCD_CA_KEY`.

Which path you take depends on one question: **does `/var/lib/etcd/member` hold
tenants?** A clone that has never served anything is re-addressed by wiping it.
A control plane that moved is not — that directory is every tenant's Patroni
state, every etcd user and role, and the `auth enable` flag.

### What editing the config does not fix

Rewriting the address in `/etc/etcd/etcd.conf.yml` is one of four places it
lives, and the other three are invisible to `grep -rn 10.10.3.66 /etc/etcd/`:

| Also holds the old address | Why the grep misses it |
|---|---|
| The server certificate's SANs | They are DER inside the `.pem`, not text. Read them with `openssl x509 -in /etc/etcd/ssl/cp-etcd-01.pem -noout -ext subjectAltName`. A node dialling the new address gets `certificate verify failed` from a certificate that still names the old one |
| The raft membership record | `initial-cluster` and `initial-advertise-peer-urls` are read at bootstrap and ignored ever after; the live peer URL is in `/var/lib/etcd/member`. Only `etcdctl member update` changes it |
| `/etc/etcd/regen.env` | If `ETCD_INITIAL_CLUSTER` is pinned there with the old address, the next boot re-renders `etcd.conf.yml` straight back to it |
| `/etc/etcd/.regen-done-<old-ip>` | The marker is per address, so the new address has none. The next boot therefore re-runs the regen script — and if the certificate still needs re-signing while `/var/lib/etcd/member` is populated, it refuses to start etcd rather than guess which of the two you meant |

`/etc/systemd/system/etcd.service.d/override.conf` is the one file that never
carries an address — it is `ExecStart=/usr/bin/etcd --config-file=…` and nothing
else. Editing it is harmless and changes nothing.

### Re-address the host first — certificate, then config

Both paths below start here, and the order matters: sealing a clone
re-bootstraps etcd from `initial-cluster`, so a data wipe done before the
address is fixed brings the new cluster straight back up advertising the old
one.

Example: `10.10.3.66` → `10.10.2.86`. Back up before anything, because
re-signing overwrites the pair in place — see
[rolling back a change to this host](#rolling-back-a-change-to-this-host):

```bash
NEW_IP=10.10.2.86
OLD_IP=10.10.3.66
NODE=$(hostname)         # the `name:` in etcd.conf.yml; also the cert filename

cp -a /etc/etcd/etcd.conf.yml /etc/etcd/etcd.conf.yml.$(date +%Y%m%dT%H%M%S)
cp -a /etc/etcd/ssl/$NODE.pem     /etc/etcd/ssl/$NODE.pem.bak
cp -a /etc/etcd/ssl/$NODE-key.pem /etc/etcd/ssl/$NODE-key.pem.bak
```

Re-sign for the new address. Keep `127.0.0.1` and `localhost` — the gRPC gateway
dials the loopback entry, and it is also the endpoint you use below while the
address is in flux — and keep the **old** address until the whole fleet has
moved off it:

```bash
cd /root/pki/etcd-ca
cat > /tmp/$NODE-csr.json <<EOF
{
  "CN": "$NODE",
  "hosts": ["$NODE", "$NEW_IP", "$OLD_IP", "127.0.0.1", "localhost"],
  "key": {"algo": "rsa", "size": 2048}
}
EOF

cfssl gencert -ca=ca.pem -ca-key=ca-key.pem \
  -config=ca-config.json -profile=server /tmp/$NODE-csr.json | cfssljson -bare $NODE
rm -f /tmp/$NODE-csr.json $NODE.csr

# Signed in the CA directory, then published to the one etcd reads.
cp $NODE.pem $NODE-key.pem /etc/etcd/ssl/

chown root:etcd /etc/etcd/ssl/$NODE.pem /etc/etcd/ssl/$NODE-key.pem
chmod 644 /etc/etcd/ssl/$NODE.pem
chmod 640 /etc/etcd/ssl/$NODE-key.pem
```

Then both files that carry the address, and the marker that is keyed to it:

```bash
sed -i "s/$OLD_IP/$NEW_IP/g" /etc/etcd/etcd.conf.yml
sed -i "s/$OLD_IP/$NEW_IP/g" /etc/etcd/regen.env   # if it pins ETCD_INITIAL_CLUSTER
rm -f /etc/etcd/.regen-done-*

grep -E 'initial-cluster|urls' /etc/etcd/etcd.conf.yml   # every address is the new one
```

On an image that carries the cloud-init's regen service, everything above is
also what `systemctl restart etcd-regen-config.service` does on its own — it
re-signs for the detected IP and re-renders the config from
`/etc/etcd/etcd.conf.yml.j2`. It stops short of the two paths below, and refuses
outright when `/var/lib/etcd/member` is populated and the certificate needed
re-signing, because that combination is a host that cannot be repaired without
someone deciding which of the two it is.

### A live control plane that moved — fix the membership record

The host keeps its data, so only the peer URL etcd advertises is stale.
`initial-cluster` does not change it — that is read at bootstrap and ignored
ever after. Start etcd and correct the record over the loopback, the one
endpoint both the old and new certificate are valid for:

```bash
systemctl restart etcd

export ETCDCTL_API=3
export ETCDCTL_ENDPOINTS=https://127.0.0.1:2379
export ETCDCTL_CACERT=/etc/etcd/ssl/ca.pem
export ETCDCTL_CERT=/etc/etcd/ssl/$NODE.pem
export ETCDCTL_KEY=/etc/etcd/ssl/$NODE-key.pem

etcdctl --user "root:<root-pw>" member list        # take the member ID from here
etcdctl --user "root:<root-pw>" member update <member-id> --peer-urls=https://$NEW_IP:2380
systemctl restart etcd

etcdctl endpoint health
```

Tenants, users, roles and the `auth enable` flag all survive — this path changes
an address and nothing else. Skip to [then the fleet](#then-the-fleet).

### A clone carrying the template's data — seal it

A clone's `/var/lib/etcd` is the *template's*: a member bootstrapped under the
template's address, holding whatever tenants, users and auth state the image was
sealed with. Resetting it gives this host a cluster of its own, with a new
cluster ID and nothing in it.

Only ever do this to a clone that has served nothing. On a control plane with
tenants it is not a repair — it destroys every tenant's Patroni state, every
etcd user and role, and the `auth enable` flag, for every cluster at once.

```bash
systemctl stop etcd

ls -la /root/etcd-backup-*/etcd-data 2>/dev/null    # anything kept from before?
cp -a /var/lib/etcd /root/etcd-data-final-backup-$(date +%Y%m%d-%H%M)
rm -rf /var/lib/etcd/*

# Must be `new`: the reset re-bootstraps, and `existing` makes etcd look for a
# cluster to join that is not there. `initial-cluster` must already name this
# node at the NEW address — that is what the re-address above was for.
grep -E 'initial-cluster-state|initial-cluster:' /etc/etcd/etcd.conf.yml

systemctl daemon-reload
systemctl start etcd
systemctl status etcd --no-pager
```

`cp -a` rather than `cp -r`: the copy is only a restorable backup if it keeps
ownership and mode (etcd will not start on a data directory it does not own —
`chown -R etcd:etcd` if you ever put one back).

An empty cluster has no root user and no RBAC, so step 8 happens again. Auth is
off at this point, so no `--user` yet:

```bash
export ETCDCTL_API=3
export ETCDCTL_ENDPOINTS=https://127.0.0.1:2379
export ETCDCTL_CACERT=/etc/etcd/ssl/ca.pem
export ETCDCTL_CERT=/etc/etcd/ssl/$NODE.pem
export ETCDCTL_KEY=/etc/etcd/ssl/$NODE-key.pem

etcdctl user add root                # prompts twice; must match CONTROL_PLANE_ETCD_ROOT_PASSWORD
etcdctl user grant-role root root
etcdctl auth enable
```

The middle command is not optional and not reorderable: `auth enable` refuses
while `root` does not hold the `root` role, and the failure reads as a
permissions error rather than a missing grant — which is why it is easy to run
`auth enable` twice and grant in between.

Then confirm the transport is the one the tenant design needs. A template built
with `client-cert-auth: false` hands out access to anyone who can reach the
port, and erawan's per-tenant certificates stop being an access control at all:

```bash
grep -n 'client-cert-auth' /etc/etcd/etcd.conf.yml
sed -i 's/client-cert-auth: false/client-cert-auth: true/' /etc/etcd/etcd.conf.yml
systemctl restart etcd
systemctl status etcd --no-pager
```

That `sed` deliberately also rewrites `peer-client-cert-auth: false` — the
pattern is a substring of it — and both belong on. Re-run step 7 afterwards:
with `client-cert-auth: true` the gateway dials etcd's own listener as a client,
so a server certificate without `clientAuth` answers every `/v3` request with
503 from here on.

Everything the old cluster held is now gone, so every tenant has to be
provisioned onto this host again — a deploy or start/recover per cluster
re-creates its role, user, key prefix and certificate. The certificate files
under `/etc/etcd/ssl/erawan-clients/` survive the wipe as files, but the etcd
users they authenticate as do not.

### Then the fleet

The address is in three more places, none of them on this host, and nothing
finds them on its own:

| Where | What to do |
|---|---|
| `SHARED_CONTROL_PLANE` in `/etc/erawan-cluster/.env` | Set it to the new IP and restart erawan. It is also the address erawan SSHes into to sign tenant certificates and run `etcdctl` |
| Every deployed cluster's `/etc/patroni/patroni.yml` | The endpoint is baked in when the file is rendered ([`patroni.yml.j2`](../cluster/pgsql/playbooks/roles/configure_node/templates/patroni.yml.j2)), and `use_proxies: true` means Patroni will never discover the new address by itself. Re-run a deploy or start/recover per cluster to re-render it |
| Node-side egress rules, if you filter them | The nodes now dial `2379` at the new address |

Until a cluster has been re-rendered its nodes are still dialling the old
address, which is why it stays in the certificate's SANs until the last one has
moved. Patroni's `ttl` is 30s: a cluster that cannot reach the DCS for longer
than that loses its leader lock and holds its primary read-only until it can.

## Rotation

| What | How | Blast radius |
|---|---|---|
| One tenant's certificate | Delete the pair under `/etc/etcd/ssl/erawan-clients/<engine>/` and re-run any deploy, start/recover or add-member for that cluster. The next run mints a new pair (stamped with the issue time) and installs it | That tenant |
| One tenant's password | Rotate on the erawan side; provisioning converges the etcd user's password on the next run | That tenant |
| Server certificate | Back up the old pair, re-sign (step 3), `systemctl restart etcd` | etcd restarts — a few seconds of DCS outage for every tenant. Patroni's `ttl` is 30s, so time it deliberately |
| CA | Effectively a rebuild: every server and tenant certificate signed by it stops being trusted, and nodes hold the old `ca.pem` until their next provisioning run. Re-issue in `/root/pki/etcd-ca`, then re-copy `ca.pem` to `/etc/etcd/ssl` | Everything |

Any change to `/etc/etcd/etcd.conf.yml` also needs an etcd restart, with the
same shared blast radius.

## Troubleshooting

Node-side symptoms — what a failing Patroni or a failing deploy looks like — are
indexed in [pgsql.md → Symptom index](pgsql.md#symptom-index). Control-plane-side:

| Seen on the control plane | Cause |
|---|---|
| `curl .../v3/auth/authenticate` returns `404` | `enable-grpc-gateway` is off, or etcd did not start from the config file you edited (`systemctl cat etcd`, and check the `--config-file` override is in effect) |
| `etcd.service` fails with `member ... has already been bootstrapped` | Leftover data in `/var/lib/etcd` from an earlier incarnation. `/var/lib/etcd/default` is the package's own auto-start and is safe to delete; `/var/lib/etcd/member` is this control plane's real state and is not |
| The host's IP changed and etcd no longer serves | The certificate's SANs, the raft membership record and every URL in `etcd.conf.yml` still name the old address. Follow [changing the control plane's private IP](#changing-the-control-planes-private-ip); do **not** clear the data directory to make it start |
| Deploy fails with `... ca-key.pem does not exist on the control plane, so a client certificate cannot be signed` | Erawan is looking for the CA key where it is not. Set `CONTROL_PLANE_ETCD_CA_KEY` to the real path (step 9) and restart erawan. The tenant's role and user were already created and converge on the re-run |
| The CA expires before the certificates it signed | `ca-csr.json` had no `ca.expiry`, so cfssl gave the CA 5 years while tenant certificates get 3650 days. `openssl x509 -in /root/pki/etcd-ca/ca.pem -noout -dates` — compare against a minted pair under `/etc/etcd/ssl/erawan-clients/` |
| `cfssl: command not found` | `apt install -y golang-cfssl` |
| Provisioning warns `authentication is not enabled` | Step 8 was skipped |
| Provisioning fails on `invalid user ID or password` for root | `CONTROL_PLANE_ETCD_ROOT_PASSWORD` does not match this host's root user |
| Nodes fail with `certificate verify failed` / IP mismatch | The server certificate has no SAN for the address the node dials — re-sign (step 3) |

Useful reads: `journalctl -u etcd -n 100`, `systemctl cat etcd` (confirms the
`--config-file` override is in effect), and
`etcdctl --user "root:<pw>" user list` / `role list` for what erawan has
provisioned.
