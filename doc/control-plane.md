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

Steps 1–9 below build one by hand, in order. Everything after them is optional:
a [cloud-init](#doing-it-at-boot-instead-the-cloud-init) that does the same work
at boot for a VM that gets rebuilt or cloned, and the operational notes —
[golden images](#building-a-golden-image),
[more members](#adding-more-control-plane-members),
[rotation](#rotation), [troubleshooting](#troubleshooting).

**Run every command as root** (`sudo -i`). Most of the files live in
`/etc/etcd/ssl`, which is not writable by a normal user, so a half-`sudo`'d
paste fails partway through.

## What you end up with

| Piece | Value used throughout this guide |
|---|---|
| Host / etcd member name | `cp-etcd-01` |
| Private IP | `10.10.3.66` |
| Client port (DB nodes, erawan) | `2379` |
| Peer port (other control-plane members only) | `2380` |
| CA | `/etc/etcd/ssl/{ca.pem,ca-key.pem,ca-config.json}` |
| Server certificate | `/etc/etcd/ssl/cp-etcd-01.pem` + `-key.pem` |
| etcd config | `/etc/etcd/etcd.conf.yml` |
| Tenant certificates (minted by erawan) | `/etc/etcd/ssl/erawan-clients/<engine>/` |
| Tenant keys | `/db/patroni/<cluster>/` |

The member name is not cosmetic: it names the server certificate, and erawan
reads that certificate by path. `CONTROL_PLANE_ETCD_CERT` / `_KEY` default to
`cp-etcd-01{,-key}.pem`, so either the host is called `cp-etcd-01` or you set
the name on both sides.

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

mkdir -p /etc/etcd/ssl /var/lib/etcd
cd /etc/etcd/ssl
```

`golang-cfssl` provides `cfssl` and `cfssljson`. `gettext-base` (for `envsubst`)
is only used by the [cloud-init](#doing-it-at-boot-instead-the-cloud-init); it
does no harm here.

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

# ca.pem = public (goes everywhere), ca-key.pem = private (stays here)
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

| File | Who needs it |
|---|---|
| `ca.pem` | etcd, erawan, and every DB node (installed as `/etc/patroni/etcd-ca.pem`) |
| `ca-key.pem` | This host only. Erawan signs tenant certificates with it **in place, over SSH** — it is never copied to a node or to the erawan host |
| `ca-config.json` | Only the host that signs certificates |

## Step 3 — Sign the server certificate

Change `CN` and `hosts` to the real node. The IP is not optional: Patroni
verifies the server certificate against **the IP it dials**, and the node-side
check erawan runs fails by name on a missing SAN rather than letting the cluster
start and fail every DCS call later. `127.0.0.1` matters for the same reason —
it is the address the gateway itself dials.

```bash
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

# Keep: ca.pem, ca-key.pem, ca-config.json, cp-etcd-01.pem, cp-etcd-01-key.pem
rm -f *.csr *-csr.json
```

## Step 4 — Set the file permissions

```bash
chown etcd:etcd /etc/etcd/ssl/*
chmod 644 /etc/etcd/ssl/*.pem
chmod 600 /etc/etcd/ssl/*-key.pem     # after the 644 — key files match *.pem too
```

Two tightenings worth making, neither of which breaks anything:

```bash
# etcd never reads the CA key; only erawan does, and it operates as root.
chown root:root /etc/etcd/ssl/ca-key.pem && chmod 600 /etc/etcd/ssl/ca-key.pem

# Once erawan has minted tenant certificates here (they are live credentials
# for other people's clusters):
chown -R root:root /etc/etcd/ssl/erawan-clients && chmod 700 /etc/etcd/ssl/erawan-clients
```

Left as `etcd:etcd`, a compromise of the etcd service account is a compromise of
the CA and of every tenant credential ever issued from it. Note also that
re-running the wildcard `chown` later re-takes the `erawan-clients` directory
for the etcd user — which is why the cloud-init sets ownership per file instead
of sweeping the directory.

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
# Defaults shown; set only if you changed the names above
# CONTROL_PLANE_ETCD_CLIENT_PORT=2379
# CONTROL_PLANE_ETCD_CACERT=/etc/etcd/ssl/ca.pem
# CONTROL_PLANE_ETCD_CERT=/etc/etcd/ssl/cp-etcd-01.pem
# CONTROL_PLANE_ETCD_KEY=/etc/etcd/ssl/cp-etcd-01-key.pem
```

Restart erawan and deploy a PostgreSQL cluster. The `control_plane_dcs` step
creates the tenant's role, user and key prefix, mints its client certificate
into `/etc/etcd/ssl/erawan-clients/pgsql/`, installs the CA and that pair on
each node, opens `2379` to those nodes, and verifies from each node that it can
authenticate. The full variable list is in the
[README configuration table](../README.md#configuration).

Existing clusters are never migrated: each job records the DCS layout it was
deployed with, so setting this variable cannot re-point a cluster that is
already running its own etcd.

## Doing it at boot instead — the cloud-init

[`cluster/shared/cloudinit/etcd-control-plane.yml`](../cluster/shared/cloudinit/etcd-control-plane.yml)
performs steps 1, 3, 5 and 6 automatically, against **the IP the VM actually
boots with**. That is the difference that matters on a platform where a rebuilt
or cloned VM comes back on a different address: the certificate from step 3 is
pinned to an IP, so a hand-built host stops serving the moment its address
changes, while a cloud-init host re-signs itself and comes up.

It cannot do step 2. A host that mints its own CA on boot would orphan every
certificate already issued to a running tenant, so the CA must be there first —
either baked into the image (the intended flow, see
[golden images](#building-a-golden-image)) or copied in afterwards:

```bash
scp ca.pem ca-key.pem ca-config.json root@<vm>:/etc/etcd/ssl/
ssh root@<vm> systemctl restart etcd-regen-config.service
```

The first boot's failure on a missing CA is expected on that path and does not
fail the rest of cloud-init.

**Do not paste the CA key into the user-data.** `write_files` with a base64 key
works, but user-data is readable from the instance metadata service by anything
running on the VM and is usually retained by the hypervisor — that is the CA for
every tenant certificate on the platform.

What `/usr/local/sbin/etcd-regen-config.sh` does on each boot:

1. detects the private IP (`ip route get 1.1.1.1`),
2. signs `/etc/etcd/ssl/<name>.pem` for it with `-profile=server`, carrying SANs
   for the node name, that IP, `127.0.0.1` and `localhost` (plus any the
   previous certificate had),
3. renders `/etc/etcd/etcd.conf.yml` from `/etc/etcd/etcd.conf.yml.j2`,
4. removes the package's `/var/lib/etcd/default`, and
5. starts etcd.

It logs to `/var/log/etcd-regen-config.log`, skips its work on a reboot at an
address it has already configured (`/etc/etcd/.regen-done-<ip>`), and is
re-runnable with `systemctl restart etcd-regen-config.service`. Node name,
cluster membership and token come from `/etc/etcd/regen.env`; the defaults are
the hostname and a single-member cluster.

The one thing it will not do is delete a bootstrapped `/var/lib/etcd/member` to
make a new address work — see [troubleshooting](#troubleshooting).

A CA can also be created on such a VM with `/usr/local/sbin/etcd-make-ca.sh`,
which runs step 2 exactly as written above and refuses to overwrite an existing
CA. It is never called at boot.

## Building a golden image

The regen script refuses to delete bootstrapped etcd data, so a template must
not carry any. Before snapshotting:

```bash
systemctl stop etcd
rm -rf /var/lib/etcd/*
rm -f /etc/etcd/.regen-done-* /etc/etcd/etcd.conf.yml
rm -f /etc/etcd/ssl/cp-etcd-01.pem /etc/etcd/ssl/cp-etcd-01-key.pem
: > /var/log/etcd-regen-config.log
```

Keep `ca.pem`, `ca-key.pem` and `ca-config.json` — inheriting the CA is the
point of the image. Every clone then signs itself a fresh certificate for its
own address on first boot.

If the image carries tenant material (it should not), clear
`/etc/etcd/ssl/erawan-clients/` too: those are live credentials for clusters
belonging to the machine you cloned.

## Adding more control-plane members

Quorum on the control plane needs an odd number of members. Add them one at a
time, and register each with the running cluster **before** it boots:

```bash
# On an existing member:
etcdctl --user "root:<root-pw>" member add cp-etcd-02 \
  --peer-urls=https://10.10.3.67:2380
```

Then, on the new VM — in `/etc/etcd/regen.env` before its first regen run, or
directly in its `etcd.conf.yml` if you are building it by hand:

```bash
ETCD_NODE_NAME=cp-etcd-02
ETCD_INITIAL_CLUSTER=cp-etcd-01=https://10.10.3.66:2380,cp-etcd-02=https://10.10.3.67:2380
ETCD_CLUSTER_STATE=existing
ETCD_CLUSTER_TOKEN=erawan-shared-cp
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

## Rotation

| What | How | Blast radius |
|---|---|---|
| One tenant's certificate | Delete the pair under `/etc/etcd/ssl/erawan-clients/<engine>/` and re-run any deploy, start/recover or add-member for that cluster. The next run mints a new pair (stamped with the issue time) and installs it | That tenant |
| One tenant's password | Rotate on the erawan side; provisioning converges the etcd user's password on the next run | That tenant |
| Server certificate | Re-sign (step 3) and `systemctl restart etcd` — or, on a cloud-init host, delete `/etc/etcd/ssl/<name>.pem`, `-key.pem` and `/etc/etcd/.regen-done-*` and restart `etcd-regen-config.service` | etcd restarts — a few seconds of DCS outage for every tenant. Patroni's `ttl` is 30s, so time it deliberately |
| CA | Effectively a rebuild: every server and tenant certificate signed by it stops being trusted, and nodes hold the old `ca.pem` until their next provisioning run | Everything |

Any change to `/etc/etcd/etcd.conf.yml` also needs an etcd restart, with the
same shared blast radius.

## Troubleshooting

Node-side symptoms — what a failing Patroni or a failing deploy looks like — are
indexed in [pgsql.md → Symptom index](pgsql.md#symptom-index). Control-plane-side:

| Seen on the control plane | Cause |
|---|---|
| `curl .../v3/auth/authenticate` returns `404` | `enable-grpc-gateway` is off, or etcd did not start from the config file you edited (`systemctl cat etcd`, and check the `--config-file` override is in effect) |
| `etcd.service` fails with `member ... has already been bootstrapped` | Leftover data in `/var/lib/etcd` from another incarnation — the package's auto-start (`/var/lib/etcd/default`, safe to delete) or a cloned image (see below) |
| `etcd-regen-config.service` failed, log says `CA files ... not found` | The CA is not on the VM yet — copy it in and restart the service |
| Log says `/var/lib/etcd/member exists, but this node's certificate had to be re-signed` | Either a clone of an unsealed image (seal it: [above](#building-a-golden-image), then restart the service), or a live control plane whose IP changed — in which case do **not** wipe: restore the old address, or recover deliberately with `etcdctl member update` or a snapshot restore |
| `cfssl: command not found` | `apt install -y golang-cfssl` |
| Provisioning warns `authentication is not enabled` | Step 8 was skipped |
| Provisioning fails on `invalid user ID or password` for root | `CONTROL_PLANE_ETCD_ROOT_PASSWORD` does not match this host's root user |
| Nodes fail with `certificate verify failed` / IP mismatch | The server certificate has no SAN for the address the node dials — re-sign (step 3) |

Useful reads: `journalctl -u etcd -n 100`, `systemctl cat etcd`,
`/var/log/etcd-regen-config.log`, and
`etcdctl --user "root:<pw>" user list` / `role list` for what erawan has
provisioned.
