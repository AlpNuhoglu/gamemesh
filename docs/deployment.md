# Deployment Guide

## 1. Docker Compose (recommended for local dev)

```bash
cp .env.example .env          # optional — dev defaults exist for everything
docker compose up --build     # one command, full stack
docker compose ps             # all services should be healthy
docker compose down           # stop (add -v to wipe the Postgres volume)
```

Boot order is handled by healthcheck-gated `depends_on`: Postgres/Redis first,
then services, then the gateway. The schema and seed SQL in `scripts/db/` are
applied automatically on first Postgres boot.

## 2. Kubernetes on k3d (local)

The manifests are a Kustomize tree: `deployments/k8s/base` holds what every
environment shares, `overlays/local` adds what the local k3d cluster needs
(locally built images, dev secrets, `gamemesh.localhost`) and
`overlays/prod` what the VPS needs. `deployments/k8s/monitoring` (Prometheus
and Grafana, namespace `monitoring`) is applied the same way everywhere. Always apply with
`-k`, never the files under `base/` directly.

### Prerequisites
- Docker, [k3d](https://k3d.io) ≥ 5.9, kubectl ≥ 1.32

### Steps

```bash
# 1. A single-node k3s cluster (the distribution the VPS runs), its API and
#    Traefik bound to localhost only; 8080 stays free for docker-compose.
k3d cluster create gamemesh --image rancher/k3s:v1.35.5-k3s1 \
  --api-port 127.0.0.1:6550 -p "127.0.0.1:18080:80@loadbalancer" --agents 0 --wait

# 2. Build every service as gamemesh/<service>:dev and load the images into
#    the cluster (no registry; the manifests use imagePullPolicy IfNotPresent)
make k8s-build

# 3. Deploy the local overlay, then monitoring: its namespace, Prometheus's
#    rights and Grafana's admin Secret (cluster-level or by hand in
#    production), and Prometheus and Grafana themselves. Refuses
#    unless kubectl points at k3d-gamemesh.
make k8s-apply

# 4. Watch it come up: 17 pods in gamemesh, Prometheus and Grafana in
#    monitoring, all 1/1
kubectl -n gamemesh get pods -w
kubectl -n monitoring get pods
```

### Accessing the stack

Through Traefik, with the Ingress host:

```bash
curl -H 'Host: gamemesh.localhost' http://127.0.0.1:18080/health
curl -H 'Host: gamemesh.localhost' http://127.0.0.1:18080/api/v1/leaderboard/top/10
# WebSocket: ws://127.0.0.1:18080/ws?token=... with Host gamemesh.localhost
```

Only `/health`, `/api` and `/ws` are routed. For the rest, port-forward:

```bash
kubectl -n monitoring port-forward svc/prometheus 9090:9090 &
kubectl -n monitoring port-forward svc/grafana 3000:3000 &   # user admin, password below
kubectl -n monitoring get secret grafana-admin -o jsonpath='{.data.password}' | base64 -d | pbcopy
```

### Useful commands

```bash
kubectl kustomize deployments/k8s/overlays/local       # what would be applied
kubectl -n gamemesh logs deploy/gateway -f             # follow gateway logs
kubectl -n gamemesh logs deploy/matchmaking -f         # watch the match loop
kubectl -n gamemesh describe pod <pod>                 # probe/image issues
kubectl -n gamemesh rollout restart deploy/player      # restart after make k8s-build
kubectl -n gamemesh exec redis-0 -- redis-cli zcard matchmaking:queue
make k8s-delete                                        # remove the local overlay
```

### Cluster settings (once per new cluster)

Settings for the k3s cluster itself, outside the overlays (the production
server sets both in its k3s config, see [Production server](#3-production-server-vps)):

```bash
# k3s installed with encryption at rest for Secrets (off by default):
#   curl -sfL https://get.k3s.io | sh -s - --secrets-encryption
k3s secrets-encrypt status              # Encryption Status: Enabled

# Traefik keeps the client's address (externalTrafficPolicy: Local); without
# it every client reaches the gateway as 10.42.0.1 and shares one rate limit.
kubectl apply -f deployments/k8s/cluster/traefik-config.yaml
```

### Production secrets

The local overlay generates `gamemesh-secrets` from
`overlays/local/.env.local`, which is gitignored and filled with random values
by `make k8s-secrets` (run by `make k8s-apply`). Postgres sets its password only
when it initialises its volume: after changing `POSTGRES_PASSWORD`, delete the
`postgres-data` PVC (local data is lost) or `ALTER USER`; otherwise the new pods
fail with `password authentication failed` while the old ones keep serving.
The production overlay generates no Secret:
the cluster must hold one, created out of band and never committed:

```bash
kubectl -n gamemesh create secret generic gamemesh-secrets \
  --from-literal=JWT_SECRET="$(openssl rand -hex 32)" \
  --from-literal=POSTGRES_USER=gamemesh \
  --from-literal=POSTGRES_PASSWORD="$(openssl rand -hex 32)" \
  --from-literal=POSTGRES_DB=gamemesh \
  --from-literal=REDIS_PASSWORD="$(openssl rand -hex 32)"
```

With `APP_ENV=production` (the base config) every service refuses to start if
a secret it uses is missing, shorter than 32 characters or a known development
default (`config.Validate`): a forgotten or weak Secret stalls the rollout and
the running pods keep serving. Rotating them: see "Rotating secrets" below.

Grafana's admin password is its own Secret, in `monitoring` (created by hand
in production too; CI only names it):

```bash
kubectl -n monitoring create secret generic grafana-admin \
  --from-literal=password="$(openssl rand -hex 32)"
```

Grafana reads it at its first start, into its database on the volume; a new
value later needs `grafana cli admin reset-admin-password` as well.

## 3. Production server (VPS)

One netcup VPS (Ubuntu 24.04, x86) runs the single-node k3s cluster. Its
address stays out of the repo; `ssh gamemesh` (an `~/.ssh/config` entry on the
admin's machine) reaches it over Tailscale.

### Rebuilding the server from nothing

In this order; each step is described in the sections below.

1. **VPS and provider firewall.** netcup VPS 500 G12.5 (x86). Create the SCP
   firewall policies first (table below), then install Ubuntu 24.04 Minimal
   from the SCP with the admin's SSH key, hostname `gamemesh-prod-1`, locale
   `en_US.UTF-8`; the key goes to root, password login off.
2. **Host.** Admin user `alp`, SSH policy, automatic updates and reboots, UTC
   ("Setup").
3. **Tailscale.** Install and join; tag the node `tag:prod` in the admin
   console (tagged nodes have no key expiry). The access policy is
   `deployments/server/tailscale-policy.hujson`. Then unassign `gamemesh-ssh`.
4. **k3s** with its config, the OIDC authentication file and the cluster-level
   manifests ("k3s", "Cluster-level manifests").
5. **Admin kubeconfig** on the admin's machine ("k3s").
6. **The Secret** `gamemesh-secrets` ("Production secrets"). New random values
   mean a new, empty database: there are no Postgres backups yet.
7. **GitHub environment `production`:** update `K8S_HOST` (the new tailnet
   address) and `K8S_CA_DATA` (the new cluster's CA); the Tailscale secrets
   stay. Run `Actions -> cluster-access`.
8. **DNS.** Point `api.gamemesh.app` (A record, DNS only) at the new public
   address.
9. **Deploy.** `Actions -> deploy` with the latest commit of `main` (or push).
   cert-manager then obtains the certificate through port 80, which needs step
   8 done. Check `curl https://api.gamemesh.app/health`.

### Provider firewall

netcup's SCP Firewall filters traffic before it reaches the VM. That matters
because Docker and k3s write their own iptables rules, which `ufw` does not
govern. It is stateful for TCP but **not for UDP**, so UDP replies need their
own rules. Every rule is INBOUND / ACCEPT; everything else is dropped:

| Policy | Rule | Why |
|---|---|---|
| `gamemesh-base` | UDP src 53 from netcup's four resolvers only | DNS replies; a reply from any other server is dropped |
| | UDP src 123 → dst 32768-60999 | NTP replies reach the kernel's ephemeral range only |
| | ICMP, ICMPv6 | ping, path MTU, IPv6 neighbour discovery |
| `gamemesh-tailscale` | UDP dst 41641 | direct WireGuard path; without it traffic falls back to a DERP relay |
| `gamemesh-web` | TCP 80, 443 | HTTP-01 challenges and the API |
| `gamemesh-ssh` | TCP 22 from one admin IP | **not assigned**; break-glass only, re-add with a current IP |

The addresses are static (netplan, no DHCP), so no DHCP rules. netcup's
default "Mail block" and "Ping allow" policies stay assigned.

### Admin access

- SSH only over the tailnet, as `alp` with a key; root logs in through `sudo`
  (which asks for `alp`'s password). The policy is
  `deployments/server/sshd_config.d/10-gamemesh.conf`.
- Break-glass: the SCP VNC console with `alp`'s password. The console uses a
  US keymap, so the password is letters and digits only (no `i`).
- The server's Tailscale node has key expiry disabled; otherwise it would
  drop off the tailnet after 180 days and leave only the console.

### Setup (once per new server)

```bash
# 0. From the admin's machine, in the repo: the two policy files
scp deployments/server/sshd_config.d/10-gamemesh.conf \
    deployments/server/apt.conf.d/52gamemesh-unattended-upgrades root@<server>:/root/

# 1. Admin user with the same key as root (run as root over SSH, in /root)
useradd -m -s /bin/bash -G sudo alp
install -d -m 700 -o alp -g alp /home/alp/.ssh
install -m 600 -o alp -g alp /root/.ssh/authorized_keys /home/alp/.ssh/authorized_keys
passwd alp

# 2. SSH policy: check before reloading, keep the root session open until a
#    new `alp` login works. sshd keeps the FIRST value it reads, and the main
#    config includes sshd_config.d at its top, so this file wins.
install -m 644 10-gamemesh.conf /etc/ssh/sshd_config.d/
sshd -t && sshd -T | grep -E '^(permitrootlogin|passwordauthentication|allowusers) '
systemctl reload ssh

# 3. Reboot after updates that need it (at 03:30 UTC, only when
#    /var/run/reboot-required exists). apt lets later files win, so 52 > 50.
install -m 644 52gamemesh-unattended-upgrades /etc/apt/apt.conf.d/
timedatectl set-timezone UTC

# 4. Tailscale from its signed apt repository; no Tailscale SSH (sshd stays)
curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg \
  | tee /usr/share/keyrings/tailscale-archive-keyring.gpg >/dev/null
curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list \
  | tee /etc/apt/sources.list.d/tailscale.list
apt-get update && apt-get install -y tailscale
tailscale up --ssh=false && tailscale set --auto-update
```

Then disable key expiry for the node in the Tailscale admin console, and
unassign `gamemesh-ssh` once `ssh gamemesh` works.

### k3s

The version is pinned to the minor the manifests are validated against
(kubeconform `1.35.0`, PSS `v1.35`); moving to a newer minor is its own step.
The settings live in `deployments/server/k3s/config.yaml`, which k3s reads on
every start: Secrets encrypted at rest, an API certificate for the tailnet
address only, and flannel's `host-gw` backend, so nothing listens on UDP 8472.

```bash
# Admin machine, in the repo: the k3s files into alp's home
scp deployments/server/k3s/config.yaml deployments/server/k3s/authentication-config.yaml \
    deployments/k8s/cluster/traefik-config.yaml deployments/k8s/cluster/cert-manager.yaml gamemesh:

# On the server, as alp, in ~. Read the installer before running it as root.
curl -sfL https://get.k3s.io -o /tmp/k3s-install.sh && sha256sum /tmp/k3s-install.sh
sudo install -d -m 755 /etc/rancher/k3s
sed "s/TAILNET_IP/$(tailscale ip -4)/" config.yaml | sudo tee /etc/rancher/k3s/config.yaml >/dev/null
sudo chmod 600 /etc/rancher/k3s/config.yaml
# Before the first start: config.yaml points the API server at this file, and
# without it the API server refuses to start (found in the rebuild drill)
sudo install -m 600 authentication-config.yaml /etc/rancher/k3s/
# Files in this directory are applied by k3s itself, on every start
sudo install -D -m 600 traefik-config.yaml /var/lib/rancher/k3s/server/manifests/traefik-config.yaml
# cert-manager (chart via k3s's helm-controller); can also be added later
sudo install -m 600 cert-manager.yaml /var/lib/rancher/k3s/server/manifests/cert-manager.yaml
sudo INSTALL_K3S_VERSION='v1.35.9+k3s1' sh /tmp/k3s-install.sh
sha256sum /usr/local/bin/k3s           # compare with the release's sha256sum-amd64.txt (arm64: sha256sum-arm64.txt)
sudo k3s secrets-encrypt status        # Encryption Status: Enabled
```

In its first seconds `secrets-encrypt status` can answer `secret-encrypt error
ID ...` (the server log says `runtime core not ready`); asked again half a
minute later it reports the status.

The admin kubeconfig (cluster-admin) is kept in its own file on the admin's
machine, never merged into `~/.kube/config` and never given to CI, so a plain
`kubectl` always talks to the local cluster:

```bash
# server: sudo install -m 600 -o alp -g alp /etc/rancher/k3s/k3s.yaml ~/k3s.yaml
scp gamemesh:k3s.yaml ~/.kube/gamemesh-prod.yaml && ssh gamemesh 'shred -u ~/k3s.yaml'
chmod 600 ~/.kube/gamemesh-prod.yaml
sed -i '' 's#https://127.0.0.1:6443#https://<tailnet-ip>:6443#' ~/.kube/gamemesh-prod.yaml
kubectl --kubeconfig ~/.kube/gamemesh-prod.yaml config rename-context default gamemesh-prod
alias kprod='kubectl --kubeconfig ~/.kube/gamemesh-prod.yaml --context gamemesh-prod'
kprod get nodes
```

### Cluster-level manifests

Everything CI must not write lives in k3s's auto-deploy directory, which k3s
applies on every start: Traefik's setting, cert-manager, the namespaces
(`gamemesh`, `monitoring`), their admission policies, Prometheus's identity
and rights, CI's RBAC and the Let's Encrypt issuers. Removing a file there
deletes what it created; the namespace files in particular must stay.
The OIDC file is for the API server itself (`kube-apiserver-arg` in
`config.yaml`), installed with k3s above; changing it needs no restart.

A rehearsal cluster without the public name (a VM, a drill) should not get
`letsencrypt-prod`: cert-manager opens orders with Let's Encrypt production for
`api.gamemesh.app` that can never validate (the name points elsewhere). Leave
both issuers out there.

```bash
# Admin machine, in the repo
scp deployments/server/k3s/authentication-config.yaml deployments/k8s/namespace/{namespace,monitoring}.yaml \
    deployments/k8s/cluster/{traefik-config,cert-manager,gamemesh-admission-policy,monitoring-admission-policy,prometheus-rbac,ci-deployer,letsencrypt-staging,letsencrypt-prod}.yaml \
    gamemesh:/tmp/
# Server, as alp
M=/var/lib/rancher/k3s/server/manifests
sudo install -D -m 600 /tmp/namespace.yaml $M/namespace-gamemesh.yaml
sudo install -m 600 /tmp/monitoring.yaml $M/namespace-monitoring.yaml
for f in traefik-config cert-manager gamemesh-admission-policy monitoring-admission-policy prometheus-rbac ci-deployer; do
  sudo install -m 600 /tmp/$f.yaml $M/$f.yaml
done
# The issuers once cert-manager runs (they need its CRDs); the repo holds a
# placeholder for the address, which is kept out of the public repo
for f in letsencrypt-staging letsencrypt-prod; do
  sed "s/ACME_EMAIL/ops@gamemesh.app/" /tmp/$f.yaml | sudo tee $M/$f.yaml >/dev/null
done
sudo chmod 600 $M/letsencrypt-*.yaml
```

### CI access to the cluster

CI holds no long-lived credential for the cluster or the tailnet. A job in the
`production` environment (deployable from `main` only):

1. joins the tailnet with `tailscale/github-action` and workload identity
   federation: GitHub's OIDC token is traded for an ephemeral `tag:ci` node;
2. asks GitHub for another OIDC token, audience `gamemesh-k8s`, lifetime 300 s;
3. calls the API server with it. The API server trusts GitHub's issuer
   (`deployments/server/k3s/authentication-config.yaml`) only for this
   repository's and owner's numeric IDs, `refs/heads/main` and environment
   `production`, and maps it to the user
   `github:repo:AlpNuhoglu/gamemesh:environment:production`;
4. RBAC (`deployments/k8s/cluster/ci-deployer.yaml`) lets that user write what
   the prod overlay holds in `gamemesh` and what `deployments/k8s/monitoring`
   holds in `monitoring`, and read pods, logs and rollouts in both. No
   Secrets, exec, delete, namespace, ServiceAccount or RBAC rights, and no
   Ingress in `monitoring`.

Around it:

- Tailscale policy (admin console): `tag:ci` may reach only `tag:prod:6443`,
  with policy tests that refuse 22, 80 and 10250; the server is tagged
  `tag:prod`.
- Trust credential (Tailscale, OpenID Connect, issuer GitHub): subject
  `repo:AlpNuhoglu/gamemesh:environment:production`, custom claim
  `repository_id`, scope Auth Keys write, tag `tag:ci`.
- Environment `production`: secrets `TS_OAUTH_CLIENT_ID` and `TS_AUDIENCE`
  (the credential's Client ID and Audience, which is
  `api.tailscale.com/<client id>`), variables `K8S_HOST` (tailnet address)
  and `K8S_CA_DATA` (the cluster CA certificate, public).
- An admission policy (`gamemesh-admission-policy.yaml`) keeps Services
  ClusterIP without externalIPs and Ingresses on class traefik and host
  `api.gamemesh.app`, whoever writes them. In `monitoring`
  (`monitoring-admission-policy.yaml`): Services ClusterIP, no Ingress at all.
- The namespaces (`deployments/k8s/namespace/`), the policies and the RBAC are
  cluster-level: copied into k3s's auto-deploy directory by the admin.

`Actions → cluster-access → Run workflow` checks all of it. If Tailscale's
token exchange fails, its first step says why (a mismatched Client ID and
Audience shows as `WARN audience is api.tailscale.com/<client id>`).

### Deploying

Every push to `main` deploys itself: after `ci-ok` and `publish`, the `deploy`
job in `.github/workflows/ci.yml` (environment `production`, one at a time,
never cancelled halfway) points the prod overlay at the commit's images,
applies it after a server-side dry run, waits for every Deployment and
StatefulSet, and waits for `https://api.gamemesh.app/health` to report the
commit. Then it applies the same commit's monitoring and waits for it; if
that fails the job is red, but the app is not rolled back. Measured on the
first one: merge to live in 6 min 46 s (CI about
4 min, publish 1.5 min, deploy 38 s), 3494 of 3494 public requests answered
200 during it.

The job logs the image that ran before it. The procedure below is the same
thing by hand, for when CI cannot run.

### Monitoring

Prometheus and Grafana run in the `monitoring` namespace
(`deployments/k8s/monitoring`). Prometheus scrapes the services' pods in `gamemesh` every 15 s and keeps 7 days, at
most 2 GB, on a 3 Gi volume. It is never exposed: its Service is ClusterIP and
the namespace's admission policy refuses any other Service type and any
Ingress. Grafana has one data source (Prometheus) and the status dashboard
(`grafana/dashboards/status.json`), both provisioned from the repository and
not editable in its UI; its settings (`grafana/grafana.ini`) turn off every
connection out of the cluster and anonymous access. It is not exposed either
(until the status page, G6). CI deploys both after the
app; they need the namespace, Prometheus's ServiceAccount and Role in
`gamemesh` ("Cluster-level manifests") and the `grafana-admin` Secret
("Production secrets") first.

To reach them, from the admin's machine:

```bash
kprod -n monitoring port-forward svc/prometheus 9090:9090    # then http://127.0.0.1:9090
# Every service scraped, one line per service:
curl -s 127.0.0.1:9090/api/v1/query --data-urlencode 'query=count by (app) (up{namespace="gamemesh"} == 1)'

kprod -n monitoring port-forward svc/grafana 3000:3000       # then http://127.0.0.1:3000, user admin
kprod -n monitoring get secret grafana-admin -o jsonpath='{.data.password}' | base64 -d | pbcopy
```

### Deploying by hand

Only images CI has published are deployed, by the commit SHA they were built
from; the overlay's `set-by-deploy` placeholder is replaced for the apply and
restored afterwards, never committed:

```bash
SHA=$(git rev-parse origin/main)       # its CI run, including publish, is green
F=deployments/k8s/overlays/prod/kustomization.yaml
sed -i '' "s/newTag: set-by-deploy/newTag: $SHA/" "$F"
kprod apply -k deployments/k8s/overlays/prod --dry-run=server   # admission, PSS included
kprod apply -k deployments/k8s/overlays/prod
git checkout -- "$F"
for w in $(kprod -n gamemesh get deploy,sts -o name); do kprod -n gamemesh rollout status "$w" --timeout=6m; done
# The server itself, not whatever DNS points at (before a DNS cutover, that is the old one):
curl --resolve api.gamemesh.app:443:<public-ip> https://api.gamemesh.app/health            # {"status":"ok","version":"<SHA>"}
curl --resolve api.gamemesh.app:443:<public-ip> -o /dev/null -w '%{http_code}\n' https://api.gamemesh.app/api/v1/leaderboard   # 200
```

Check the apply's own output for errors rather than a summary of it: a
validation failure (for example when the OpenAPI download times out over a bad
tailnet path) applies nothing, and the rollout checks that follow still report
success for the unchanged Deployments.

The `gamemesh-secrets` Secret must exist first (see "Production secrets"); its
namespace comes from the auto-deploy directory ("Cluster-level manifests").

After a reboot everything comes back by itself in about 40 s (measured:
reboot to Traefik answering, 44 s). The node's `Ready` condition is not
evidence of that: the API serves the last stored status before the kubelet
reports again.

### Rolling back

- **Automatic.** When a deploy does not come up (rollout stuck for 5 minutes,
  or the smoke test fails: `/health` must report the commit and
  `/api/v1/leaderboard` must answer 200), the job redeploys the commit that ran
  before, with that commit's own manifests and images, and ends red. Its log
  says `rolled back: <previous> runs again`. Tested with a deliberately broken
  release: the old gateway pod kept serving and never restarted.
- **By hand.** `Actions -> deploy -> Run workflow` (branch `main`) with the full
  SHA of the commit to run. It must be on `main`; its images were published by
  that commit's CI run. Commits from `7b36b40` (#76) on can be deployed this
  way; older overlays still contain the namespace, which CI may not write.
- Which SHA: each deploy log starts with `running before: <sha>`;
  `curl https://api.gamemesh.app/health` shows what runs now; `git log
  --first-parent main` lists the merges.
- What it does not undo: cluster-level manifests (they are not in CI's hands)
  and data. A release that changed the database schema needs its own way back.

### Rotating secrets

Only the admin can (CI has no access to Secrets). Services read the Secret at
start (`envFrom`), so a change takes effect when they restart. After each
change, check its shape without printing it:

```bash
kprod -n gamemesh get secret gamemesh-secrets -o json | python3 -c 'import base64, json, sys
for k, v in sorted(json.load(sys.stdin)["data"].items()): print(k, len(base64.b64decode(v)))'
```

- **`JWT_SECRET`** signs every login token: changing it logs every user out
  (all tokens stop verifying).
  ```bash
  kprod -n gamemesh patch secret gamemesh-secrets --type merge \
    -p "{\"stringData\":{\"JWT_SECRET\":\"$(openssl rand -hex 32)\"}}"
  kprod -n gamemesh rollout restart deploy/gateway deploy/player deploy/websocket
  ```
- **`REDIS_PASSWORD`** is Redis's `requirepass`, read at start. Redis and every
  client must restart together; until all have, Redis-backed routes fail
  readiness for a few seconds.
  ```bash
  kprod -n gamemesh patch secret gamemesh-secrets --type merge \
    -p "{\"stringData\":{\"REDIS_PASSWORD\":\"$(openssl rand -hex 32)\"}}"
  kprod -n gamemesh rollout restart sts/redis deploy/gateway deploy/player deploy/websocket \
    deploy/leaderboard deploy/matchmaking deploy/presence
  ```
- **`POSTGRES_PASSWORD`** is stored by Postgres in its data directory; the
  Secret only seeded it at first start. Change it in the database first, then
  in the Secret, then restart its users. Open connections keep working until
  then.
  ```bash
  NEW=$(openssl rand -hex 32)
  kprod -n gamemesh exec deploy/postgres -- psql -U gamemesh -d gamemesh \
    -c "ALTER USER gamemesh PASSWORD '$NEW'"
  kprod -n gamemesh patch secret gamemesh-secrets --type merge \
    -p "{\"stringData\":{\"POSTGRES_PASSWORD\":\"$NEW\"}}"
  unset NEW
  kprod -n gamemesh rollout restart deploy/player deploy/outbox-relay
  ```

### If a secret leaks

1. **Rotate it first** (above). Deleting the leaked copy is not enough: it has
   been read.
2. **Contain the effect.** A new `JWT_SECRET` ends every session, the attacker's
   too. (Rotating without logging everyone out would need a key ID in the token
   header and two keys accepted for a while; not built.)
3. **Look for use** in the logs from the time of the leak: logins, unusual
   client addresses (the gateway logs `client_ip`).
4. **Only then, optionally, rewrite history** (`git filter-repo`). Forks and
   clones keep the old commits, which is why rotation comes first. GitHub's
   push protection and the CI's gitleaks job are there to stop it earlier.

### Troubleshooting

**The deploy job is red.** The log says which step:

| Message | Meaning |
|---|---|
| `images for <sha> are not published` | `publish` failed or the SHA is wrong; the cluster was not touched |
| an error from `--dry-run=server` | admission refused it: `in gamemesh, ...` is the admission policy, `violates PodSecurity` is Pod Security, `forbidden: User "github:..."` is CI's RBAC |
| `exceeded its progress deadline` | the new pods never became ready; see "A pod does not come up" |
| `serving <other sha>` until it gives up | pods are ready but the public endpoint is not: Ingress, TLS, DNS |
| `rolled back: <sha> runs again` | the previous commit runs; fix forward or keep it |
| `ROLLBACK ... FAILED` | redeploy a known good SHA with `Actions -> deploy`, or by hand |

**CI cannot reach the cluster.** Run `Actions -> cluster-access`. Its first
step checks the Tailscale inputs (`WARN audience is api.tailscale.com/<client
id>` means the Client ID and Audience do not belong together). After a
rebuild, `K8S_HOST` and `K8S_CA_DATA` must be updated.

**A pod does not come up.**

```bash
kprod -n gamemesh get pods                    # READY 0/1 vs RESTARTS climbing
kprod -n gamemesh describe pod <pod>          # probe failures, OOMKilled, image pull
kprod -n gamemesh logs <pod> --previous       # the run that crashed
kprod -n gamemesh get endpointslices          # which pods a Service sends traffic to
```

- `READY 0/1` with steady `RESTARTS`: the process runs but readiness fails,
  usually a dependency (Redis, Postgres, NATS) is unreachable.
- `RESTARTS` climbing, `CrashLoopBackOff`: the process exits by itself (for
  example `config.Validate` refusing a missing or weak secret: the reason is in
  `logs --previous`) or liveness kills it.
- Exit code 137 is SIGKILL (also an out-of-memory kill: `OOMKilled` in
  `describe`); 143 is SIGTERM; 1 is the program's own error.

**The certificate is not renewed.** cert-manager renews 30 days before expiry
(at two thirds of 90 days); nothing alerts before it expires (finding #85).

```bash
kprod -n gamemesh get certificate,certificaterequest,order,challenge
kprod -n gamemesh describe challenge
kprod -n gamemesh get events --field-selector type=Warning
```

- The challenge stays `pending` with a failed self check: port 80 not open
  (provider firewall), DNS not pointing at the server, or the HTTP-01 path
  redirected (only the API's routes are).
- An event `in gamemesh, ...`: the admission policy refused the solver's
  Service or Ingress (the issuers must ask for `serviceType: ClusterIP`).
- CAA allows only Let's Encrypt; another CA cannot issue. Let's Encrypt's
  production limits are tight: try with `letsencrypt-staging` first.

**kubectl from the admin's machine is slow or times out.** Large UDP packets on
the tailnet path from home were lost twice (finding #83): `tailscale ping`
takes hundreds of milliseconds and 1200-byte pings are lost. Restarting the
home router fixed it; meanwhile `ssh gamemesh` and `sudo k3s kubectl` on the
server work.

## 4. Grafana dashboard import (non-provisioned Grafana)

1. Grafana → **Dashboards → New → Import**
2. Upload `config/grafana/dashboards/gamemesh.json`
3. Select your Prometheus datasource → **Import**

In the compose stack this happens automatically via provisioning.

## 5. CI/CD

`.github/workflows/ci.yml` runs on every pull request, every push to `main`,
nightly and by hand. The `main` ruleset requires one check, `ci-ok`, which
passes only if every job below passed:

| Job | What it checks |
|---|---|
| `lint` | golangci-lint; the Dockerfile's Go matches `go.mod`'s toolchain; compose and Kubernetes pin the same third-party images |
| `test`, `integration` | unit tests with `-race`; Testcontainers against real Postgres and Redis |
| `services` | lists `cmd/*` for the image matrix |
| `docker` | builds each service image and scans it with Trivy (end-of-life base images fail too) |
| `scan-deps` | Trivy on the repository: misconfigurations, vulnerabilities, secrets |
| `gitleaks` | the whole history for committed secrets |
| `third-party-images`, `scan-third-party` | Trivy on every pinned third-party image |
| `manifests` | renders both overlays and validates them with kubeconform (custom resources against a pinned CRDs-catalog commit) |
| `govulncheck` | Go vulnerabilities reachable from our code |

Accepted findings are in `.trivyignore.yaml`, each scoped to a path, package or
file and each with an expiry. On pushes to `main` two more jobs run after
`ci-ok`: `publish` pushes `ghcr.io/alpnuhoglu/gamemesh-<service>:<sha>` for
amd64 and arm64, and `deploy` deploys the commit (section 3, "Deploying").

Two more workflows, both by hand: `cluster-access` checks CI's access to the
cluster, and `deploy` deploys a chosen commit of `main` (rolling back).
Dependabot proposes updates for Go modules, actions and pinned images.
