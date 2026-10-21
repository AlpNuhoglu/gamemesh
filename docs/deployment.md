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
(locally built images, dev secrets, monitoring, `gamemesh.localhost`) and
`overlays/prod` what the VPS needs. Always apply an overlay with `-k`, never
the files under `base/` directly.

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

# 3. Deploy the local overlay (refuses unless kubectl points at k3d-gamemesh)
make k8s-apply            # kubectl apply -k deployments/k8s/overlays/local

# 4. Watch it come up: 19 pods, all 1/1
kubectl -n gamemesh get pods -w
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
kubectl -n gamemesh port-forward svc/prometheus 9090:9090 &
kubectl -n gamemesh port-forward svc/grafana 3000:3000 &
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
the running pods keep serving.

(or External Secrets Operator / Sealed Secrets / a cloud secret manager).

## 3. Production server (VPS)

One netcup VPS (Ubuntu 24.04, x86) runs the single-node k3s cluster. Its
address stays out of the repo; `ssh gamemesh` (an `~/.ssh/config` entry on the
admin's machine) reaches it over Tailscale.

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
# 1. Admin user with the same key as root (run as root over SSH)
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
# On the server, as alp. Read the installer before running it as root.
curl -sfL https://get.k3s.io -o /tmp/k3s-install.sh && sha256sum /tmp/k3s-install.sh
sudo install -d -m 755 /etc/rancher/k3s
sed "s/TAILNET_IP/$(tailscale ip -4)/" config.yaml | sudo tee /etc/rancher/k3s/config.yaml >/dev/null
sudo chmod 600 /etc/rancher/k3s/config.yaml
# Files in this directory are applied by k3s itself, on every start
sudo install -D -m 600 traefik-config.yaml /var/lib/rancher/k3s/server/manifests/traefik-config.yaml
# cert-manager (chart via k3s's helm-controller); can also be added later
sudo install -m 600 cert-manager.yaml /var/lib/rancher/k3s/server/manifests/cert-manager.yaml
sudo INSTALL_K3S_VERSION='v1.35.9+k3s1' sh /tmp/k3s-install.sh
sha256sum /usr/local/bin/k3s           # compare with the release's sha256sum-amd64.txt
sudo k3s secrets-encrypt status        # Encryption Status: Enabled
```

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
   the prod overlay holds in `gamemesh`, and read pods, logs and rollouts.
   No Secrets, exec, delete, namespace or RBAC rights.

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
  `api.gamemesh.app`, whoever writes them.
- The namespace (`deployments/k8s/namespace/`), the policy and the RBAC are
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
commit. Measured on the first one: merge to live in 6 min 46 s (CI about
4 min, publish 1.5 min, deploy 38 s), 3494 of 3494 public requests answered
200 during it.

The job logs the image that ran before it. The procedure below is the same
thing by hand, for when CI cannot run.

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
curl http://api.gamemesh.app/health      # {"status":"ok","version":"<SHA>"}
```

Check the apply's own output for errors rather than a summary of it: a
validation failure (for example when the OpenAPI download times out over a bad
tailnet path) applies nothing, and the rollout checks that follow still report
success for the unchanged Deployments.

The `gamemesh-secrets` Secret must exist first (see "Production secrets"); the
namespace is created by hand for it, and the first apply adds its labels.

After a reboot everything comes back by itself in about 40 s (measured:
reboot to Traefik answering, 44 s). The node's `Ready` condition is not
evidence of that: the API serves the last stored status before the kubelet
reports again.

## 4. Grafana dashboard import (non-provisioned Grafana)

1. Grafana → **Dashboards → New → Import**
2. Upload `config/grafana/dashboards/gamemesh.json`
3. Select your Prometheus datasource → **Import**

In the compose stack this happens automatically via provisioning.

## 5. CI/CD

`.github/workflows/ci.yml` runs on every push/PR:

1. **Lint** — golangci-lint
2. **Test** — unit tests with `-race` + coverage artifact
3. **Integration** — Testcontainers against real Postgres/Redis
4. **Build** — all five binaries
5. **Docker** — buildx matrix build of all five images (GHA-cached)
6. **Security** — Trivy fs scan (HIGH/CRITICAL fail the build) + govulncheck
7. **Deploy (example)** — client-side `kubectl apply --dry-run` validation of
   the manifests; swap in a real `kubectl apply` with cluster credentials.
