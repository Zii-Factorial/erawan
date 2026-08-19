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

A cloud-init that does most of it is in the repo:
[`cluster/shared/cloudinit/etcd-control-plane.yml`](../cluster/shared/cloudinit/etcd-control-plane.yml).
It cannot do the CA, because a host that mints its own CA on boot would orphan
every certificate already issued to a running tenant. So the order is: make the
CA (step 1), put it on the VM (step 2), boot (step 3).

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
the name on both sides (`ETCD_NODE_NAME` in `/etc/etcd/regen.env`, and the two
erawan variables).

## Prerequisites

- **A VM with etcd 3.4.** Ubuntu 24.04 packages `etcd-server`
  `3.4.30-1ubuntu0.24.04.3` — the version the four requirements in
  [pgsql.md](pgsql.md#configuring-the-control-plane) were verified against, and
  what the cloud-init assumes. Older Ubuntu LTS releases ship the 3.3 series,
  which nothing here is tested against.
- **Network.** DB nodes reach `2379`; erawan opens that port per tenant with UFW
  during provisioning, scoped to the tenant's node addresses. `2380` is needed
  only between control-plane members. Nothing needs to be public.
- **SSH from the erawan host as root** (or a user that can read
  `/etc/etcd/ssl/ca-key.pem` and run `etcdctl`). It defaults to the cluster SSH
  credentials; `CONTROL_PLANE_SSH_USER` / `_PRIVATE_KEY_PATH` / `_PORT`
  override. Provisioning runs `etcdctl` and signs tenant certificates *on this
  host* — the CA key never leaves it.
- **Decide the failure domain first.** Patroni's `ttl` is 30s, so a control
  plane down longer than that costs **every** tenant its leader lock and demotes
  every primary to read-only until it returns. One member is fine for a lab and
  is a shared single point of failure in production; see
  [multi-member](#adding-more-control-plane-members) below for what erawan does
  and does not do with more than one.

## Step 1 — Create the CA

Once, on the first control plane. Everything on every control plane — server
certificates and every tenant certificate erawan ever mints — is signed by this
one CA, so a second control-plane host gets a *copy* of these three files, never
its own CA.

```bash
apt-get install -y golang-cfssl
mkdir -p /etc/etcd/ssl && cd /etc/etcd/ssl
```

The signing profile first. The `client auth` usage in the **server** profile is
the load-bearing part and the easiest thing to leave out:

```bash
cat > ca-config.json <<'EOF'
{
  "signing": {
    "default": { "expiry": "87600h" },
    "profiles": {
      "server": {
        "expiry": "87600h",
        "usages": ["signing", "key encipherment", "server auth", "client auth"]
      }
    }
  }
}
EOF
```

Under `client-cert-auth: true` the gRPC JSON gateway dials etcd's own gRPC
listener **as a client**, presenting this same certificate. A server certificate
without `clientAuth` therefore answers every `/v3` request with HTTP 503
`error reading server preface: remote error: tls: bad certificate`, while
`etcdctl` and `/version` keep working — see
[pgsql.md requirement 3](pgsql.md#3-the-control-planes-server-certificate-needs-clientauth).

Then the CA itself:

```bash
cat > ca-csr.json <<'EOF'
{
  "CN": "erawan-etcd-ca",
  "key": { "algo": "rsa", "size": 2048 },
  "names": [ { "O": "erawan" } ],
  "ca": { "expiry": "175200h" }
}
EOF

cfssl gencert -initca ca-csr.json | cfssljson -bare ca
rm -f ca.csr ca-csr.json

chmod 600 ca-key.pem
chmod 644 ca.pem ca-config.json
```

`cfssl gencert -initca` writes `ca.pem` and `ca-key.pem` (and a `ca.csr` that is
of no further use). The CA expiry is deliberately longer than the certificates
it signs: a CA that expires under live tenants invalidates all of them at once.

| File | Who needs it |
|---|---|
| `ca.pem` | etcd, erawan, and every DB node (installed as `/etc/patroni/etcd-ca.pem`) |
| `ca-key.pem` | This host only. Erawan signs tenant certificates with it **in place, over SSH** — it is never copied to a node or to the erawan host |
| `ca-config.json` | Only the host that signs server certificates (the boot-time re-sign in step 3 uses it) |

The same three files are what
`/usr/local/sbin/etcd-make-ca.sh` writes if you would rather run one command on
a VM the cloud-init has already built. It refuses to overwrite an existing CA.

## Step 2 — Put the CA on the VM

The boot-time configuration in step 3 fails by name when these files are
missing, so pick one of:

1. **Bake it into the image.** Place the three files in `/etc/etcd/ssl` on the
   template VM and snapshot it (see [sealing](#building-a-golden-image) — an
   image with leftover etcd data will refuse to start). Every clone comes up
   fully configured with no manual step. This is the flow the cloud-init is
   written for.
2. **Copy it in after the first boot.** Let cloud-init run, `scp` the files to
   `/etc/etcd/ssl`, then:

   ```bash
   systemctl restart etcd-regen-config.service
   ```

   The first boot's failure is expected on this path and does not fail the rest
   of cloud-init.

**Do not paste the CA key into the cloud-init user-data.** `write_files` with a
base64 CA key works, but user-data is readable from the instance metadata
service by anything running on the VM and is usually retained by the hypervisor
— that is the CA for every tenant certificate on the platform.

## Step 3 — Boot the VM with the cloud-init

Pass [`cluster/shared/cloudinit/etcd-control-plane.yml`](../cluster/shared/cloudinit/etcd-control-plane.yml)
as user-data. On first boot it installs `etcd-server`, `etcd-client` and
`golang-cfssl`, then runs `/usr/local/sbin/etcd-regen-config.sh`, which:

1. detects the private IP (`ip route get 1.1.1.1`),
2. signs `/etc/etcd/ssl/<name>.pem` for that IP with `-profile=server`, carrying
   SANs for the node name, the detected IP, `127.0.0.1` and `localhost` (plus
   any SANs the previous certificate had),
3. renders `/etc/etcd/etcd.conf.yml` from `/etc/etcd/etcd.conf.yml.j2`,
4. drops `/var/lib/etcd/default` — the member directory the package's own
   auto-start leaves behind, which is never this node's real state, and
5. starts etcd.

It is idempotent and re-runnable (`systemctl restart etcd-regen-config.service`),
logs everything to `/var/log/etcd-regen-config.log`, and skips its work on a
reboot at an address it has already configured
(`/etc/etcd/.regen-done-<ip>` marks it).

Because the address is detected rather than configured, a clone of a sealed
image comes up correctly on whatever IP it is given — which is what makes this
survivable on a platform that hands out a new address every time a VM is
rebuilt.

The one thing it will not do is delete a **bootstrapped** member directory
(`/var/lib/etcd/member`) to make a new address work. That data is every tenant's
Patroni state, every etcd user and role, and the `auth enable` flag; erasing it
to clear a startup error would take out every cluster on the platform at once.
It stops with instructions instead — see
[troubleshooting](#troubleshooting).

Then continue at step 6. Steps 4 and 5 are for a host you are configuring by
hand.

## Step 4 — Sign the server certificate by hand

Only if you are not using the cloud-init. Same profile, same SANs:

```bash
cd /etc/etcd/ssl
cat > cp-etcd-01-csr.json <<'EOF'
{
  "CN": "cp-etcd-01",
  "hosts": ["cp-etcd-01", "10.10.3.66", "127.0.0.1", "localhost"],
  "key": { "algo": "rsa", "size": 2048 }
}
EOF

cfssl gencert \
  -ca=ca.pem -ca-key=ca-key.pem -config=ca-config.json -profile=server \
  cp-etcd-01-csr.json | cfssljson -bare cp-etcd-01

rm -f cp-etcd-01.csr cp-etcd-01-csr.json
chown root:etcd cp-etcd-01.pem cp-etcd-01-key.pem
chmod 644 cp-etcd-01.pem
chmod 640 cp-etcd-01-key.pem
```

The IP in `hosts` is not optional: Patroni verifies the server certificate
against **the IP it dials**, and the node-side check erawan runs fails by name
on a missing SAN rather than letting the cluster start and fail every DCS call
later.

Write `/etc/etcd/etcd.conf.yml` from the reference config in
[pgsql.md](pgsql.md#reference-etcetcdetcdconfyml) — or copy the template out of
the cloud-init — and make sure `enable-grpc-gateway: true` is really in it. etcd
defaults that flag to `true` on the command line and **`false`** under
`--config-file`, and nothing warns you: `/version` and `etcdctl` answer either
way, while Patroni gets `404 page not found` and dies on
`AttributeError: 'int' object has no attribute 'get'`.

## Step 5 — Verify the certificate

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

A missing `TLS Web Client Authentication` is a `ca-config.json` without
`"client auth"` in the server profile (step 1), and re-signing is the fix —
fixing the profile alone changes nothing already issued.

## Step 6 — Create the root user and enable RBAC

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

## Step 7 — Prove the gateway is serving

The obvious smoke test (`curl https://10.10.3.66:2379/version`) is answered by
etcd's own HTTP handler and stays green even when the API Patroni uses is
entirely unavailable. Use the v3 path instead:

```bash
curl -s -o /dev/null -w '%{http_code}\n' \
  --cacert /etc/etcd/ssl/ca.pem \
  --cert /etc/etcd/ssl/cp-etcd-01.pem \
  --key /etc/etcd/ssl/cp-etcd-01-key.pem \
  -X POST https://10.10.3.66:2379/v3/auth/authenticate -d '{}'
```

| Code | Meaning |
|---|---|
| `404` | Gateway off. Fix `enable-grpc-gateway` and restart etcd |
| `400` | Gateway **live**. This is the CN guard firing on the server certificate's own CommonName, which is expected here — tenant certificates are minted without one |
| `503` | Server certificate has no `clientAuth` (step 5) |

Erawan re-runs the equivalent check from every DB node on every deploy,
start/recover and add-member, and fails the job with the cause named — so this
is a pre-flight, not the only line of defence.

## Step 8 — Point erawan at it

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

If the image is also meant to carry tenant material (it should not), clear
`/etc/etcd/ssl/erawan-clients/` too: those are live credentials for clusters
that belong to the machine you cloned.

## Adding more control-plane members

Quorum on the control plane needs an odd number of members. Add them one at a
time, and register each with the running cluster **before** it boots:

```bash
# On an existing member:
etcdctl --user "root:<root-pw>" member add cp-etcd-02 \
  --peer-urls=https://10.10.3.67:2380
```

Then, on the new VM before its first `etcd-regen-config` run (bake it into the
image's `/etc/etcd/regen.env`, or write it and re-run the service):

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
| Server certificate | Delete `/etc/etcd/ssl/<name>.pem`, `-key.pem` and `/etc/etcd/.regen-done-*`, run `systemctl restart etcd-regen-config.service` | etcd restarts — a few seconds of DCS outage for every tenant. Patroni's `ttl` is 30s, so time it deliberately |
| CA | Effectively a rebuild: every server and tenant certificate signed by it stops being trusted, and nodes hold the old `ca.pem` until their next provisioning run | Everything |

Any change to `/etc/etcd/etcd.conf.yml` also needs an etcd restart, with the
same shared blast radius.

## Troubleshooting

Node-side symptoms — what a failing Patroni or a failing deploy looks like — are
indexed in [pgsql.md → Symptom index](pgsql.md#symptom-index). Control-plane-side:

| Seen on the control plane | Cause |
|---|---|
| `etcd-regen-config.service` failed, log says `CA files ... not found` | Step 2 — the CA is not on the VM yet. Copy it in and re-run the service |
| Log says `/var/lib/etcd/member exists, but this node's certificate had to be re-signed` | Either a clone of an unsealed image (seal it: [above](#building-a-golden-image), then re-run the service), or a live control plane whose IP changed — in which case do **not** wipe: restore the old address, or recover deliberately with `etcdctl member update` or a snapshot restore |
| `etcd.service` fails with `member ... has already been bootstrapped` | Same cause: leftover data from another incarnation |
| `cfssl: command not found` in the log | `apt-get install -y golang-cfssl` |
| `curl .../v3/auth/authenticate` returns `404` | `enable-grpc-gateway` is off or the config file was not the one etcd started with (`systemctl cat etcd`) |
| Provisioning warns `authentication is not enabled` | Step 6 was skipped |
| Provisioning fails on `invalid user ID or password` for root | `CONTROL_PLANE_ETCD_ROOT_PASSWORD` does not match this host's root user |

Useful reads: `/var/log/etcd-regen-config.log`, `journalctl -u etcd -n 100`,
`systemctl cat etcd` (confirms the `--config-file` override is in effect), and
`etcdctl --user "root:<pw>" user list` / `role list` for what erawan has
provisioned.
