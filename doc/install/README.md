# Production Installation Guides

This folder contains OS-specific production install guides for the API host:
- `ubuntu.md`
- `debian.md`
- `rocky.md`

PostgreSQL deployment note:

- PostgreSQL Patroni/etcd can be deployed as a single primary node or as a primary with one or more standby nodes.
- `standby_ips` may be empty for a small single-node deployment.

MySQL deployment note:

- MySQL InnoDB Cluster can be deployed as a single primary node or as a primary with one or more secondary nodes.
- This deployment does not use MySQL Router; every node runs a lightweight primary-check HTTP endpoint (`:9200` by default) that HAProxy uses to find the current Group Replication primary.

SSH access note:

- Recommended default is SSH key authentication with a dedicated sudo-capable user.
- Set `CLUSTER_SSH_USER` and `CLUSTER_SSH_PRIVATE_KEY_PATH` in `/etc/erawan-cluster/.env`.
- Place the matching private key on the API host before restarting the service.
- These guides assume the cloud image or template already trusts the corresponding public key on each DB node.

All guides use the same production layout:

```text
/usr/local/bin/erawan-cluster                 # API binary
/opt/erawan-cluster/cluster/                  # Ansible playbooks
/etc/erawan-cluster/.env                      # API config (root-owned, group-readable)
/var/lib/erawan-cluster/cluster/jobs/         # MySQL job state
/var/lib/erawan-cluster/cluster/jobs/pgsql/   # PostgreSQL job state (default)
/var/lib/erawan-cluster/haproxy/tenants/      # Generated HAProxy tenant configs
/etc/systemd/system/erawan-cluster.service    # API systemd unit
```

## Security baseline
1. Run API as non-root (`erawan` user from installer scripts).
2. Set strong `API_KEY` in `/etc/erawan-cluster/.env`.
3. Restrict API network exposure (private subnet or firewall allowlist only).
4. Keep MySQL and SSH credentials out of shell history and Postman exports.
5. Keep HAProxy reload permission minimal:
   - `erawan ALL=(root) NOPASSWD: /bin/systemctl reload haproxy`
6. Keep file permissions strict:
   - `/etc/erawan-cluster/.env` as `0640` and owned by `root:erawan`
   - `/var/lib/erawan-cluster` as `0750`
7. For internet-exposed environments, terminate TLS in front of API.

## HAProxy rollout behavior
- Installers configure HAProxy for hot reload.
- Runtime updates should use `reload` (no restart) to avoid dropping active connections.

## HTTPS / TLS termination

The API's HTTP server never terminates TLS itself (see [security.md](../security.md#network-exposure)).
Keep `API_HOST=127.0.0.1` and put a TLS-terminating reverse proxy in front. Since
HAProxy is already installed by these guides, terminate TLS there rather than adding
a second HTTP daemon (leaner footprint, no extra process to patch on a host that
already runs Ansible jobs and DB TCP-proxying).

### Before you start

- **Make sure nothing else owns port 80/443.** Some base images ship a default
  nginx. Check first:
  ```bash
  sudo ss -tlnp | grep -E ':80 |:443 '
  ```
  If nginx (or anything else) shows up and isn't serving something you need, remove
  it — it silently wins the bind and every step below will fail in confusing ways
  (certbot times out, HAProxy reload errors) otherwise:
  ```bash
  sudo systemctl stop nginx
  sudo systemctl disable nginx
  sudo apt purge -y nginx nginx-common nginx-core nginx-full 2>/dev/null
  sudo apt autoremove -y
  sudo rm -rf /etc/nginx /var/log/nginx
  ```

- **Know where to put HAProxy config so it actually loads.** The installer's
  systemd override starts HAProxy with exactly:
  ```
  haproxy -Ws -f /etc/haproxy/haproxy.cfg -f /var/lib/erawan-cluster/haproxy/tenants ...
  ```
  Only those two `-f` targets are read. A file dropped anywhere else under
  `/etc/haproxy/` (e.g. `/etc/haproxy/erawan-cluster.cfg`) is silently ignored. Put
  your config **inside the tenants directory** instead — it's already wired up and
  needs no systemd changes:
  ```bash
  sudo nano /var/lib/erawan-cluster/haproxy/tenants/00-erawan-cluster.cfg
  ```
  No `global`/`defaults` section is needed in that file — `haproxy.cfg` already
  defines those and they carry over to blocks declared in files loaded after it.

- **Comment out entire blocks, never just the header line.** HAProxy has no
  concept of a "disabled block" — a `#` only blanks that one line. If you
  comment `#backend erawan_api` but leave `server api1 127.0.0.1:8080 check`
  active underneath, that `server` line doesn't vanish — it silently attaches
  to whatever section was last opened *above* it in the file (e.g.
  `backend acme_challenge`). Seen in practice: this turned `acme_challenge`
  into a two-server backend that load-balanced ACME HTTP-01 validation
  requests between `certbot` and the live API, causing intermittent renewal
  failures with no obvious error in the HAProxy log. When toggling a block
  during hand-edits, comment (or delete) every line in it, not just the
  `frontend`/`backend` header.

### Public domain (Let's Encrypt via certbot)

Requires a DNS A-record pointing a domain at this host's public IP with ports 80/443
reachable from the internet — verify with the cloud firewall/security group, not
just locally; a host can be listening and still be unreachable if the security
group never opened 80/443.

1. `sudo apt install -y certbot`

2. Add the ACME-challenge routing first (the `:443` block needs a cert that doesn't
   exist yet, so it comes later — adding it now will fail HAProxy's config check).
   Write the **complete** file in one go; a partial paste (frontend without its
   backend) fails with `unable to find required default_backend`:
   ```
   frontend erawan_http
       bind *:80
       acl is_acme path_beg /.well-known/acme-challenge/
       use_backend acme_challenge if is_acme
       redirect scheme https code 301 unless is_acme

   backend acme_challenge
       server certbot 127.0.0.1:8888
   ```
   Validate and reload:
   ```bash
   sudo haproxy -c -f /etc/haproxy/haproxy.cfg -f /var/lib/erawan-cluster/haproxy/tenants
   sudo systemctl reload haproxy
   sudo ss -tlnp | grep ':80 '   # must show haproxy, not nginx or anything else
   ```
   Confirm external reachability before spending a certbot attempt (run from
   somewhere off this host):
   ```bash
   curl -I http://api.yourdomain.com/.well-known/acme-challenge/test
   ```
   Any HTTP response (404 included) is fine — a timeout means the cloud firewall is
   still blocking inbound 80, fix that before continuing.

3. Issue the cert. `--http-01-port` only controls where *certbot's own* temporary
   server listens locally — Let's Encrypt itself always connects to port 80
   externally, which is why step 2's HAProxy routing has to be live first:
   ```bash
   sudo certbot certonly --standalone --http-01-port 8888 -d api.yourdomain.com
   ```

4. Combine cert + key into the single PEM HAProxy expects. **Don't lock this down
   to `600`/root-only** — the erawan-cluster API (running as the unprivileged
   `erawan` user) re-validates the *entire* tenants directory with its own
   `haproxy -c` subprocess on every future `/haproxy/config/*` call, including
   ones for completely unrelated DB clusters, and that subprocess needs to open
   this file too. `600`/root-only causes every subsequent HAProxy config creation
   to fail with a confusing `cannot open the file '.../erawan-api.pem'` error —
   the *live* HAProxy daemon (root, via systemd) can still read it fine, so
   manual `sudo haproxy -c` / `sudo systemctl reload` checks won't reveal the
   problem; it only surfaces the next time the API itself validates a change:
   ```bash
   sudo mkdir -p /etc/haproxy/certs
   sudo chgrp erawan /etc/haproxy/certs
   sudo chmod 750 /etc/haproxy/certs
   cat /etc/letsencrypt/live/api.yourdomain.com/fullchain.pem \
       /etc/letsencrypt/live/api.yourdomain.com/privkey.pem \
       | sudo tee /etc/haproxy/certs/erawan-api.pem
   sudo chgrp erawan /etc/haproxy/certs/erawan-api.pem
   sudo chmod 640 /etc/haproxy/certs/erawan-api.pem
   # confirm the API's service user can actually read it before moving on
   sudo -u erawan cat /etc/haproxy/certs/erawan-api.pem > /dev/null && echo "OK: erawan can read it"
   ```

5. Now append the HTTPS frontend + backend to the same tenants file from step 2:
   ```
   frontend erawan_api_https
       bind *:443 ssl crt /etc/haproxy/certs/erawan-api.pem
       http-response set-header Strict-Transport-Security "max-age=31536000; includeSubDomains"
       default_backend erawan_api

   backend erawan_api
       option forwardfor
       server api1 127.0.0.1:8080 check
   ```
   `option forwardfor` matters: without it, HAProxy doesn't set `X-Forwarded-For`,
   so `middleware.RealIP` in the app logs every request as `127.0.0.1` instead of
   the real client IP.

6. Validate, reload, confirm:
   ```bash
   sudo haproxy -c -f /etc/haproxy/haproxy.cfg -f /var/lib/erawan-cluster/haproxy/tenants
   sudo systemctl reload haproxy
   sudo ss -tlnp | grep -E ':443 '
   curl -v https://api.yourdomain.com/health
   ```

7. Automate renewal — certbot's package already installs a systemd timer
   (`systemctl list-timers | grep certbot`). Add a deploy-hook so the combined PEM
   and HAProxy reload happen automatically on every renewal:
   ```bash
   sudo tee /etc/letsencrypt/renewal-hooks/deploy/haproxy-reload.sh <<'EOF'
   #!/bin/bash
   cat /etc/letsencrypt/live/api.yourdomain.com/fullchain.pem \
       /etc/letsencrypt/live/api.yourdomain.com/privkey.pem \
       > /etc/haproxy/certs/erawan-api.pem
   chgrp erawan /etc/haproxy/certs/erawan-api.pem
   chmod 640 /etc/haproxy/certs/erawan-api.pem
   systemctl reload haproxy
   EOF
   sudo chmod +x /etc/letsencrypt/renewal-hooks/deploy/haproxy-reload.sh
   sudo certbot renew --dry-run
   ```
   Keep `chgrp erawan` + `chmod 640` in this hook, not `chmod 600` — otherwise every
   renewal quietly resets the permission fix from step 4 and the next
   `/haproxy/config/*` call fails again.

8. **Close the plaintext port — don't rely on `API_HOST=127.0.0.1` alone.** Confirm
   the app really is loopback-only, then firewall `:8080` as a second layer so a
   future config slip can't silently re-expose it:
   ```bash
   sudo ss -tlnp | grep :8080          # must show 127.0.0.1:8080, never 0.0.0.0:8080
   curl -m 5 http://<public-ip>:8080/health   # from off-host; must time out
   sudo ufw deny 8080/tcp
   ```
   Also check the cloud security group has no rule forwarding public `:8080` to
   this instance — only `80`/`443` should be open. Mass internet scanners probe
   exposed ports within minutes (a real example seen in testing: a bot hit
   `/SDK/webLanguage`, a known IoT-exploit scan signature, moments after the port
   was briefly open), so verify this rather than assume it.

9. Update every client's endpoint config from the old plaintext
   `http://<ip>:8080` to `https://api.yourdomain.com` (no port needed — HTTPS
   defaults to 443) once step 8 confirms 8080 is no longer reachable.

### Private/internal only (no public domain)

Let's Encrypt requires public reachability. Use an internal CA (preferred if more
than one internal client must trust the cert long-term) or a self-signed cert for
test environments:

```bash
sudo mkdir -p /etc/haproxy/certs
sudo chgrp erawan /etc/haproxy/certs
sudo chmod 750 /etc/haproxy/certs
openssl req -x509 -nodes -newkey rsa:2048 -days 825 \
  -keyout /etc/haproxy/certs/erawan-api.key \
  -out /etc/haproxy/certs/erawan-api.crt \
  -subj "/CN=<internal-ip-or-hostname>" \
  -addext "subjectAltName=IP:<internal-ip>"
cat /etc/haproxy/certs/erawan-api.crt /etc/haproxy/certs/erawan-api.key \
  | sudo tee /etc/haproxy/certs/erawan-api.pem
sudo chgrp erawan /etc/haproxy/certs/erawan-api.pem
sudo chmod 640 /etc/haproxy/certs/erawan-api.pem
sudo -u erawan cat /etc/haproxy/certs/erawan-api.pem > /dev/null && echo "OK: erawan can read it"
```
Same reasoning as the public-domain path: `600`/root-only breaks the API's own
config-validation subprocess (runs as `erawan`), not the live HAProxy daemon — see
step 4 above for the full explanation.

Add the same HTTPS frontend/backend block as step 5 above (skip the ACME/`:80`
pieces — they're only for Let's Encrypt). Distribute `erawan-api.crt` to internal
clients that need to trust it; otherwise they must skip certificate verification,
which weakens MITM protection on the private network — document that tradeoff if
you accept it. No Let's-Encrypt-style renewal automation applies here; track the
cert's expiry manually (or shorten `-days` and put a reminder on the calendar).
Steps 8–9 above (firewall `:8080`, update client endpoints) still apply.
