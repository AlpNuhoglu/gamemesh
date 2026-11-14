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

Only `/` (exactly), `/health`, `/api` and `/ws` are routed. For the rest, port-forward:

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
6. **The Secrets** `gamemesh-secrets` ("Production secrets") and
   `gamemesh-backup` ("Backups"). New random values mean a new, empty
   database. To keep the data, restore the latest backups instead ("Restoring
   after losing the server"): the Secrets come back with the cluster's state.
7. **GitHub environment `production`:** update `K8S_HOST` (the new tailnet
   address) and `K8S_CA_DATA` (the new cluster's CA; after a restore the CA is
   the old one, so it should not change); the Tailscale secrets stay. Run
   `Actions -> cluster-access`.
8. **DNS.** Point `api.gamemesh.app` and `status.gamemesh.app` (A records, DNS
   only) at the new public address.
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

#### Image garbage collection

Every deploy pulls eight new images, and the kubelet's defaults remove unused
ones only when the disk is 85 % full. Three weeks and about a hundred deploys
after the first one, images were 17.7 GiB of the 27.6 GiB in use, 45 % of the
61.7 GiB disk (2026-11-13; Prometheus's data was 74 MiB and the pods' logs
20 MiB). `deployments/server/k3s/kubelet.conf.d/10-image-gc.conf` changes
that: an image unused for a week is removed whatever the disk's state, and if
the disk still reaches 60 % the kubelet collects down to 50 %. The age limit
is the one that keeps the disk empty; thresholds alone would hold it between
50 and 60 %. Rollbacks are unaffected: the previous release is a week old at
most in normal use, and an older one pulls its images again.

The kubelet reads the file at start, so it takes a restart of k3s (pods keep
running; the API is away for some seconds). The age limit has no command-line
flag, which is why this is a kubelet configuration drop-in and not a
`kubelet-arg` in `config.yaml`. Tried on k3d with k3s 1.35.5: the three values
are picked up and the file survives the restart.

```bash
# Admin machine, in the repo
scp deployments/server/k3s/kubelet.conf.d/10-image-gc.conf gamemesh:
# Server, as alp
sudo install -D -m 644 10-image-gc.conf /var/lib/rancher/k3s/agent/etc/kubelet.conf.d/10-image-gc.conf
sudo systemctl restart k3s
# Admin machine: 60, 50 and 168h0m0s
kprod get --raw /api/v1/nodes/gamemesh-prod-1/proxy/configz |
  jq '.kubeletconfig | {imageGCHighThresholdPercent, imageGCLowThresholdPercent, imageMaximumGCAge}'
```

The kubelet forgets when an image was last used when it restarts, so the
first removals come a week after the restart, not at once.

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
   Secrets, exec, delete, namespace, ServiceAccount or RBAC rights. In
   `monitoring` its Ingress is held to the status page by the admission
   policy.

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
  (`monitoring-admission-policy.yaml`): Services ClusterIP, and Ingresses only
  for `status.gamemesh.app`, only the status page's paths to the `grafana`
  Service (or an ACME challenge to cert-manager's solver).
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
commit and for a join on `/ws` to be answered ("The smoke test's account").
Then it applies the same commit's monitoring and waits for it; if
that fails the job is red, but the app is not rolled back. Measured on the
first one: merge to live in 6 min 46 s (CI about
4 min, publish 1.5 min, deploy 38 s), 3494 of 3494 public requests answered
200 during it.

The job logs the image that ran before it. The procedure below is the same
thing by hand, for when CI cannot run.

#### The smoke test's account

`/ws` carries the game, and the HTTP checks never reach it. After them the
smoke test (`.github/actions/deploy/ws_smoke.py`) logs in, opens `/ws` with
the token, sends a `join` for a room that does not exist, needs the answer
within 5 s, closes and logs out; five tries, 5 s apart. A release that breaks
the upgrade, the token or session check, or the read or write loop of the
WebSocket service is rolled back like any other failed deploy.

It logs in as a fixed account, `benchsmoke`, which exists for nothing else and
which the admin creates once, by hand. The deploy never registers it: sign-up
is not the smoke test's to check, and a broken sign-up must not fail a deploy.
Its password is the `production` environment's secret `SMOKE_PASSWORD`: 8 to
72 characters, printable ASCII, no space at either end.

```bash
read -rs "PW?benchsmoke password: "; echo
jq -n --arg p "$PW" '{username:"benchsmoke", email:"bench_smoke@latencybench.invalid", password:$p}' |
  curl -sS -X POST https://api.gamemesh.app/api/v1/auth/register -H 'Content-Type: application/json' -d @- | jq .username
printf '%s' "$PW" | gh secret set SMOKE_PASSWORD --env production; unset PW
```

Both are checked before the cluster is touched. With the secret empty, or the
login refused, the deploy stops with `smoke account missing or wrong password`
and changes nothing. (The API answers 401 for a missing account and for a
wrong password alike.) If the login fails any other way, production may
already be down, which is when a deploy is wanted: a warning, and it goes on.

**Either answer to the join passes, and that must stay so.** Releases with the
room membership check answer a stranger's join with `JoinRejected`; releases
before it accepted any join and broadcast `PlayerJoined`. A rollback runs this
same script against the previous commit, so a check that required
`JoinRejected` would report a working rollback to an older release as failed.

What it does not prove: that the room lookup in Redis works. A lookup that
fails is answered with the same `JoinRejected`, on purpose without a reason;
`gamemesh_ws_join_rejected_total{reason="unavailable"}` is where that shows.
A change to the login answer, to `?token=` or to the `join` action has to
change this check in the same pull request, and stay compatible with the
release before it.

### Monitoring

Prometheus and Grafana run in the `monitoring` namespace
(`deployments/k8s/monitoring`). Prometheus scrapes the services' pods in `gamemesh` every 15 s and keeps 7 days, at
most 2 GB, on a 3 Gi volume. Once a minute it also reads the node's disk
usage from the kubelet, keeping only those series ("Disk"). It is never exposed: its Service is ClusterIP and
the namespace's admission policy refuses any other Service type and any
Ingress. Grafana has one data source (Prometheus) and the status dashboard
(`grafana/dashboards/status.json`), both provisioned from the repository and
not editable in its UI; its settings (`grafana/grafana.ini`) turn off every
connection out of the cluster and anonymous access. Its admin side is not
exposed; only the status page is ("Status page"). CI deploys both after the
app; they need the namespace, Prometheus's ServiceAccount and Role in
`gamemesh` ("Cluster-level manifests") and the `grafana-admin` Secret
("Production secrets") first.

**Two files to keep in step by hand.** CI applies Prometheus's configuration
(`monitoring/prometheus.yml`) but may not write RBAC, so what Prometheus is
allowed to read (`cluster/prometheus-rbac.yaml`) reaches the server only when
the admin copies it ("Cluster-level manifests"). A scrape job that needs a new
right therefore stays without targets, logging `... is forbidden`, until that
file is copied again; nothing else is affected, and it starts working without
a restart. The `node-disk` job needs the ClusterRole `prometheus-node-metrics`
from that file:

```bash
scp deployments/k8s/cluster/prometheus-rbac.yaml gamemesh:/tmp/
# -t: sudo on the server asks for alp's password and needs a terminal for it
ssh -t gamemesh 'sudo install -m 600 /tmp/prometheus-rbac.yaml /var/lib/rancher/k3s/server/manifests/prometheus-rbac.yaml && rm /tmp/prometheus-rbac.yaml'
kprod get clusterrolebinding prometheus-node-metrics    # k3s applies the directory within seconds
```

To reach them, from the admin's machine:

```bash
kprod -n monitoring port-forward svc/prometheus 9090:9090    # then http://127.0.0.1:9090
# Every service scraped, one line per service:
curl -s 127.0.0.1:9090/api/v1/query --data-urlencode 'query=count by (app) (up{namespace="gamemesh"} == 1)'

kprod -n monitoring port-forward svc/grafana 3000:3000       # then http://127.0.0.1:3000, user admin
kprod -n monitoring get secret grafana-admin -o jsonpath='{.data.password}' | base64 -d | pbcopy
```

### Disk

Everything the cluster writes is on one partition of the node: images, logs
and every volume. The volumes' sizes (1 Gi, 3 Gi) are requests, not limits:
`local-path` volumes are directories on that partition, so one that grows
takes the room of all the others.

Two things watch it. Every hour the `disk-check` CronJob
(`deployments/k8s/overlays/prod/disk-check.yaml`) reads the usage and reports
to two checks at Healthchecks.io: `disk-warning` fails at 70 %, `disk-critical`
at 85 %, and either e-mails `ops@gamemesh.app`; a run that does not happen is
noticed there too ("The dead man's switch"). And Prometheus keeps the figure
(job `node-disk`, from the kubelet), shown on the status dashboard's "Node
disk used" panel with lines at the same two levels. The panel tells nobody;
the checks do.

The CronJob needs the two ping URLs, given on standard input like the
backups':

```bash
read -rs "WARN?disk-warning ping URL: "; echo; read -rs "CRIT?disk-critical ping URL: "; echo
printf '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"gamemesh-disk-check","namespace":"gamemesh"},"stringData":{"DISK_WARN_PING_URL":"%s","DISK_CRIT_PING_URL":"%s"}}' "$WARN" "$CRIT" |
  kprod apply -f -; unset WARN CRIT
kprod -n gamemesh create job --from=cronjob/disk-check disk-check-now   # then its log, and both checks turn green
```

Without the Secret the job fails every hour and its log says which URL is
missing; nothing else is affected.

When a check fails: `kprod get --raw /api/v1/nodes/gamemesh-prod-1/proxy/stats/summary | jq '.node.fs, .node.runtime.imageFs'`
shows the disk and the images' share. Images are the usual cause ("Image
garbage collection"); `sudo k3s crictl rmi --prune` on the server removes the
unused ones at once.

### Status page

`https://status.gamemesh.app` shows the status dashboard to anyone, read-only
and without login. It is Grafana's shared ("public") dashboard; Grafana has
no anonymous user at all.

- **What is routed** (`deployments/k8s/monitoring/status.yaml`): only what the
  page loads, measured with a headless browser: `/public-dashboards/<token>`,
  `/api/public/dashboards/<token>/...` (the share's definition and its panels'
  stored queries) and `/public/` (static files); `/` redirects to the page.
  Everything else on that host is a 404 from Traefik and never reaches
  Grafana: its login, admin and query APIs, `/metrics`, other shares. The
  namespace's admission policy refuses any other Ingress there.
- **The share** lives in Grafana's database. The `publisher` container in
  Grafana's pod creates it with the token written in `grafana.yaml`, and puts
  it back if it is switched off, changed or replaced; its log has one line per
  change (`kprod -n monitoring logs deploy/grafana -c publisher`). The token is
  the page's URL, public by design. To change it, change all four places
  (`status.yaml` and `grafana.yaml`; `scripts/k8s/check-status-token.sh`, run
  by CI, fails otherwise).
- **Load:** 10 requests/s per client address, burst 100 (a page's first load
  is about 47 requests, then 5 every 10 s); beyond that, 429. Viewers cannot
  change the time range or run their own queries: a panel query runs the
  query stored in the dashboard, whatever the request says.
- **TLS:** cert-manager and `letsencrypt-prod`, like the API (ECDSA P-256, 90
  days, renewed 30 days before it ends). It was tried with
  `letsencrypt-staging` first; on a new cluster without the public name, leave
  the issuer out ("Cluster-level manifests").

### Backups

Every night at 02:15 UTC the `postgres-backup` CronJob
(`overlays/prod/backup.yaml`, image from `deployments/backup`) dumps the
database (`pg_dump --format=custom`), encrypts it with
[age](https://age-encryption.org) for the admin's **public** key and uploads it
to the Cloudflare R2 bucket `gamemesh-backups` as
`postgres/gamemesh-<UTC time>.dump.age`. The server can write a backup but not
read one: the private key is offline, with the admin. The bucket's lock keeps
every object for 30 days, so not even a leaked token can delete or overwrite
it; a lifecycle rule deletes it after 90 days.

Once, before the first deploy that carries the CronJob:

1. **R2 (Cloudflare dashboard → R2):** create the bucket `gamemesh-backups`
   (Standard storage); in its settings add a **bucket lock** rule (no prefix,
   so all objects; retention 30 days, never "Indefinite", which would keep
   every object forever) and an **object lifecycle** rule on the prefix
   `postgres/` (delete after 90 days; R2's default multipart-abort rule
   already holds the empty prefix, and E3's backups get their own). Then
   *Manage API tokens → Create API token*: permission **Object Read & Write**,
   applied to `gamemesh-backups` only. Keep the Access Key ID, the Secret Access
   Key and the S3 endpoint (`https://<account id>.r2.cloudflarestorage.com`).
2. **The age key pair**, without installing anything on the admin's machine:
   ```bash
   docker run --rm alpine:3.24 sh -c 'apk add -q age && age-keygen'
   ```
   It prints a `Public key: age1...` line and an `AGE-SECRET-KEY-...` line.
   The secret line goes into the password manager **and** an offline copy; it
   is the only way to read a backup. Only the public key goes to the cluster.
   Saved next to it, the public key line needs a leading `#`: age reads every
   other line of a key file as a key.
3. **The Secret**, typed in without leaving the values in the shell history:
   ```bash
   read -r  "R2_ID?Access Key ID: ";        read -rs "R2_KEY?Secret Access Key: "; echo
   read -r  "R2_EP?S3 endpoint: ";          read -r  "AGE_PUB?age public key: "
   kprod -n gamemesh create secret generic gamemesh-backup \
     --from-literal=RCLONE_CONFIG_R2_ACCESS_KEY_ID="$R2_ID" \
     --from-literal=RCLONE_CONFIG_R2_SECRET_ACCESS_KEY="$R2_KEY" \
     --from-literal=RCLONE_CONFIG_R2_ENDPOINT="$R2_EP" \
     --from-literal=AGE_RECIPIENT="$AGE_PUB"
   unset R2_ID R2_KEY R2_EP AGE_PUB
   ```
   (`read "VAR?prompt"` is zsh; in bash, `read -p "prompt" VAR`.) The dead man's
   switch's URL is added afterwards as `PING_URL` ("The dead man's switch").
4. CI's RBAC (`ci-deployer.yaml`) must allow CronJobs before that deploy
   ("Cluster-level manifests"); otherwise the apply is refused and the deploy
   rolls back.

Run one now, and check it:

```bash
kprod -n gamemesh create job --from=cronjob/postgres-backup backup-manual-$(date +%s)
kprod -n gamemesh logs -f job/<that name>      # ... "uploaded r2:gamemesh-backups/postgres/..."
kprod -n gamemesh get cronjob,jobs
```

Then check that the private key opens it; a backup only the wrong key could
read is no backup. Download the object from the dashboard (it saves as
`postgres_gamemesh-<time>.dump.age`) and, with the key pasted at the prompt
(not from the clipboard: copying this command replaces it):

```bash
F="$HOME/Downloads/postgres_gamemesh-<time>.dump.age"
read -rs "AGEKEY?age secret key: "; echo
printf '%s\n' "$AGEKEY" | docker run --rm -i --mount type=bind,src="$F",dst=/b.age,readonly alpine:3.24 sh -c '
apk add -q age postgresql16-client >/dev/null 2>&1
grep -io "AGE-SECRET-KEY-1[0-9A-Z]*" > /tmp/key || { echo "no AGE-SECRET-KEY in the input"; exit 1; }
age --decrypt -i /tmp/key /b.age | pg_restore --list | head -25'
unset AGEKEY
```

It lists the archive (`dbname: gamemesh`, the tables `players`,
`player_stats`, `outbox_events` and their data). `--mount` rather than `-v`:
given a path that does not exist, `-v` mounts a new empty directory there
instead of failing.

Restoring one onto a new server, with the cluster's state, is "Restoring after
losing the server" below. Restoring over the live database is a decision to
take with the application stopped, and has not been tried.

#### The cluster's own state

Every night at 02:30 UTC a systemd timer on the server
(`deployments/server/backup`) backs up k3s's datastore, the SQLite file that
holds every Kubernetes object, Secrets included. It takes a consistent copy
while k3s runs (SQLite's online backup), checks it (`integrity_check`, and
that k3s's bootstrap entry is there), compresses it and pipes it into a pod
of the same backup image, which encrypts it for the same age public key and
uploads it as `k3s/k3s-<UTC time>.db.gz.age`. The service runs as root with
no capabilities and a read-only file system but for its own directory; the
pod runs under the namespace's restricted rules, like the CronJob's.

The datastore also holds, encrypted with the **server token**, the cluster's
CA keys and the key that encrypts the Secrets. The token is never uploaded:
it is kept by the admin, so the bucket and the age key together still do not
open a Secret. Without the token, a backup of the datastore restores nothing.

Once, on top of the Postgres backup's setup:

1. **R2:** an object lifecycle rule on the prefix `k3s/`, deleting after 31
   days (the lock keeps them 30; only the newest matters for a restore). A
   second API token, **Object Read & Write** on `gamemesh-backups` only, for
   this backup alone.
2. **The server token**, into the password manager and the offline copy,
   without showing it on screen. Two machines, one block each:
   ```bash
   # Server, as alp
   sudo install -m 600 -o alp -g alp /var/lib/rancher/k3s/server/token ~/k3s-token
   ```
   ```bash
   # Admin machine (pbcopy and the gamemesh host name exist only there)
   ssh gamemesh 'cat ~/k3s-token' | pbcopy && ssh gamemesh 'shred -u ~/k3s-token'
   # paste it into the password manager entry (one line: K10...::server:...), then
   pbcopy </dev/null
   ```
   It changes only if the token is rotated; save it again then.
3. **The Secret** for the pod, as for the CronJob's: the new R2 token's keys
   and endpoint, and the same age **public** key (`age1...`). That one is not
   on R2's token page; it is the key pair's public line, or, without showing
   it, from the Postgres backup's Secret:
   `kprod -n gamemesh get secret gamemesh-backup -o jsonpath='{.data.AGE_RECIPIENT}' | base64 -d | pbcopy`
   (run in a second terminal while the prompt below waits).
   ```bash
   read -r  "R2_ID?Access Key ID: ";        read -rs "R2_KEY?Secret Access Key: "; echo
   read -r  "R2_EP?S3 endpoint: ";          read -r  "AGE_PUB?age public key: "
   kprod -n gamemesh create secret generic gamemesh-k3s-backup \
     --from-literal=RCLONE_CONFIG_R2_ACCESS_KEY_ID="$R2_ID" \
     --from-literal=RCLONE_CONFIG_R2_SECRET_ACCESS_KEY="$R2_KEY" \
     --from-literal=RCLONE_CONFIG_R2_ENDPOINT="$R2_EP" \
     --from-literal=AGE_RECIPIENT="$AGE_PUB"
   unset R2_ID R2_KEY R2_EP AGE_PUB
   ```
4. **The server:** sqlite3 from Ubuntu (main), the script and the units (the
   script reports a failure with `curl`, which Ubuntu Server already has):
   ```bash
   # Admin machine, in the repository's directory (the paths are relative to it)
   scp deployments/server/backup/{k3s-backup.sh,gamemesh-k3s-backup.service,gamemesh-k3s-backup.timer} gamemesh:
   ```
   ```bash
   # Server, as alp, in ~
   sudo apt-get install -y sqlite3
   sudo install -m 755 k3s-backup.sh /usr/local/sbin/gamemesh-k3s-backup
   sudo install -m 644 gamemesh-k3s-backup.service gamemesh-k3s-backup.timer /etc/systemd/system/
   sudo systemctl daemon-reload
   sudo systemctl enable --now gamemesh-k3s-backup.timer
   rm k3s-backup.sh gamemesh-k3s-backup.service gamemesh-k3s-backup.timer
   systemctl list-timers --no-pager gamemesh-k3s-backup.timer     # NEXT: 02:30 UTC
   ```
   age and rclone stay off the host: Ubuntu's are built with go1.22.2 and
   never rebuilt; the image's are CI's.

Run one now, and check it:

```bash
# Server
sudo systemctl start gamemesh-k3s-backup.service   # returns when done; non-zero if it failed
sudo journalctl -u gamemesh-k3s-backup.service -n 20 -o cat --no-pager
#   copied ...: <n> rows, integrity ok / received <n> bytes, sha256 as expected / uploaded r2:gamemesh-backups/k3s/...
```

`journalctl` needs `sudo`: as alp it shows a system service's log as
nothing at all. And `--no-pager` on both: without it they open `less`, and
the rest of a pasted block becomes its keystrokes (an `s` among them saves
the screen to a file named after what follows).

Then that the private key opens it, as for the Postgres backup (the dashboard
saves it as `k3s_k3s-<time>.db.gz.age`):

```bash
F="$HOME/Downloads/k3s_k3s-<time>.db.gz.age"
read -rs "AGEKEY?age secret key: "; echo
printf '%s\n' "$AGEKEY" | docker run --rm -i --mount type=bind,src="$F",dst=/b.age,readonly alpine:3.24 sh -c '
apk add -q age sqlite >/dev/null 2>&1
grep -io "AGE-SECRET-KEY-1[0-9A-Z]*" > /tmp/key || { echo "no AGE-SECRET-KEY in the input"; exit 1; }
age --decrypt -i /tmp/key /b.age | gunzip > /tmp/state.db
sqlite3 /tmp/state.db "PRAGMA integrity_check; SELECT count(*) FROM kine WHERE name LIKE '"'"'/bootstrap/%'"'"';"'
unset AGEKEY
```

It prints `ok` and `1`. Restoring it is the next section.

#### The dead man's switch

A backup that stops running makes no noise by itself. Each backup therefore
reports to a check on Healthchecks.io (the ops account), and the check sends
an e-mail to `ops@gamemesh.app` when the report is bad or missing:

| Check | Expects | Reported by |
|---|---|---|
| `postgres-backup` | cron `15 2 * * *` UTC, grace 30 minutes | the CronJob's pod |
| `k3s-backup` | cron `30 2 * * *` UTC, grace 30 minutes | the upload pod; on failure also the server's script |
| `cert-check` | simple: once a day, grace 12 hours | the `cert-check` workflow |
| `disk-warning` | cron `5 * * * *` UTC, grace 30 minutes | the `disk-check` CronJob's pod: fails at 70 % |
| `disk-critical` | cron `5 * * * *` UTC, grace 30 minutes | the same pod: fails at 85 % |

`cert-check` is not on a cron schedule because GitHub starts this
repository's scheduled workflows 7 to 8.5 hours late (measured; its 06:00
run starts around 14:00 UTC).

Each check has its own ping URL. The URL is a secret: whoever knows it can
report a success that never happened. It is in no file of this repository
and is not pasted into a terminal's history or a chat. For the two backups
it is the `PING_URL` key of their Secrets, given on standard input:

```bash
read -rs "PING?postgres-backup ping URL: "; echo
printf '{"stringData":{"PING_URL":"%s"}}' "$PING" |
  kprod -n gamemesh patch secret gamemesh-backup --type merge --patch-file /dev/stdin; unset PING
```

and the same with `k3s-backup`'s URL for `gamemesh-k3s-backup`. For the
workflow it is the repository secret `HC_PING_CERT`.

What a run reports:

- **Success:** a plain ping. The job's log ends with `pinged the dead man's
  switch`.
- **Failure:** the URL with `/fail`, and `reported the failure to the dead
  man's switch` in the log. The e-mail says "received a failure signal".
  Nothing about the failure is sent; the reason is in the log (below). The
  Postgres job tries three times, so a failure that a later try repairs
  gives a DOWN and then an UP e-mail.
- **Nothing:** a run that never started (node down, CronJob suspended,
  timer disabled) or was killed (a dump stuck past the job's 15 minutes is
  killed without a report). The e-mail says the ping "did not arrive on
  time" and comes when the grace has passed.

A ping that cannot be sent does not change a run's result: the backup is in
the bucket, and the check goes down for the missing ping.

When an e-mail comes, read the run's log first:

```bash
kprod -n gamemesh get cronjob,jobs                 # was there a job last night, and how did it end?
kprod -n gamemesh logs job/<the job's name>
# Server, for the k3s backup
systemctl list-timers --no-pager gamemesh-k3s-backup.timer
sudo journalctl -u gamemesh-k3s-backup.service -n 30 -o cat --no-pager
```

After the cause is fixed, run the backup by hand (the commands above); its
ping turns the check green again.

**Trying the alert** (done on 2026-11-10; the e-mail came within ten
seconds of the failing run). Neither test touches the CronJob, the
timer or the data. For Postgres, a copy of the CronJob's job with a database
host that does not exist and a single try, then a normal run:

```bash
kprod -n gamemesh create job --from=cronjob/postgres-backup k3-fail --dry-run=client -o json \
  | jq '.spec.backoffLimit=0 | (.spec.template.spec.containers[0].env[] | select(.name=="PGHOST")).value="no-such-host"' \
  | kprod -n gamemesh create -f -
kprod -n gamemesh logs job/k3-fail      # ... reported the failure to the dead man's switch
# after the DOWN e-mail ("received a failure signal"):
kprod -n gamemesh create job --from=cronjob/postgres-backup k3-ok      # UP e-mail
kprod -n gamemesh delete job k3-fail
```

For the k3s backup, the service itself with a datastore path that does not
exist, given by a drop-in under `/run` (gone at the next boot, but remove
it: while it is there every backup fails):

```bash
# Server
sudo mkdir -p /run/systemd/system/gamemesh-k3s-backup.service.d
printf '[Service]\nEnvironment=K3S_DB=/nonexistent\n' | sudo tee /run/systemd/system/gamemesh-k3s-backup.service.d/k3-test.conf >/dev/null
sudo systemctl daemon-reload
sudo systemctl start gamemesh-k3s-backup.service; echo "exit $?"          # exit 1
sudo journalctl -u gamemesh-k3s-backup.service -n 6 -o cat --no-pager     # ... reported the failure ...
# after the DOWN e-mail:
sudo rm -r /run/systemd/system/gamemesh-k3s-backup.service.d && sudo systemctl daemon-reload
systemctl show -p Environment gamemesh-k3s-backup.service                 # Environment= (empty)
sudo systemctl start gamemesh-k3s-backup.service; echo "exit $?"          # exit 0, UP e-mail
```

#### Restoring after losing the server

**What this assumes:** the server is gone, but the R2 bucket, the age private
key and the server token are intact. Without any one of the three, this
restores nothing: the bucket holds the only copies, the key opens them, and
the token opens the datastore's CA keys and the Secrets' encryption key.
Rehearsed on an empty VM in the phase 2 plan's E5 (2026-11-08): the cluster,
its Secrets and the players came back, less what is listed at the end.

First the rebuild's steps 1-3 ("Rebuilding the server from nothing"): the VPS
with the hostname `gamemesh-prod-1`, the host, Tailscale. The hostname matters:
k3s names the node after it, and the restored pods and volumes belong to that
node. Then, in place of steps 4 and 6:

1. **k3s, installed but not started**, with its files and every cluster-level
   manifest in place as in "k3s" and "Cluster-level manifests" (the issuers'
   CRDs come back with the datastore, so their files can go in at once). The
   first start runs without the agent: the API server alone, no pod.
   ```bash
   # Server, as alp: the installer as in "k3s", but
   sudo INSTALL_K3S_VERSION='v1.35.9+k3s1' INSTALL_K3S_SKIP_START=true sh /tmp/k3s-install.sh
   sudo install -d -m 755 /etc/rancher/k3s/config.yaml.d
   echo 'disable-agent: true' | sudo tee /etc/rancher/k3s/config.yaml.d/90-restore.yaml >/dev/null
   sudo apt-get install -y sqlite3
   ```
   `disable-agent` is an experimental flag that `k3s server --help` does not
   list; v1.35.9 accepts it.
2. **The backups and the token**, from the admin's machine. Download the
   newest `postgres/` and `k3s/` objects from the dashboard, then decrypt them
   there and stream them over ssh: the private key never reaches the server
   and no decrypted copy lands on the admin's disk. Paste the block, then the
   key at its prompt, then the token at its prompt:
   ```bash
   # Admin machine, in ~/Downloads
   read -rs "AGEKEY?age secret key: "; echo
   for f in k3s_k3s-<time>.db.gz.age postgres_gamemesh-<time>.dump.age; do
     printf '%s\n' "$AGEKEY" | docker run --rm -i --mount type=bind,src="$PWD/$f",dst=/b.age,readonly alpine:3.24 sh -c '
       apk add -q age >/dev/null 2>&1
       grep -io "AGE-SECRET-KEY-1[0-9A-Z]*" > /tmp/key || { echo "no AGE-SECRET-KEY in the input" >&2; exit 1; }
       age --decrypt -i /tmp/key /b.age' | ssh gamemesh "umask 077; mkdir -p ~/restore && cat > ~/restore/${f%.age}" && echo "$f: ok"
   done
   unset AGEKEY
   read -rs "K3STOKEN?server token: "; echo
   printf '%s\n' "$K3STOKEN" | ssh gamemesh 'umask 077; cat > ~/restore/token' && echo "token: ok"
   unset K3STOKEN
   ```
   The drill streamed into the VM's root directly (its user needed no sudo
   password); into alp's home, as here, is the same pipe without the sudo.
3. **Check what arrived** before using it. A transfer can be cut short
   without an error: in the drill, `multipass exec` stopped a 4.5 MB stream at
   2 MB and exited 0; ssh delivered it whole.
   ```bash
   # Server, in ~/restore
   gzip -t k3s_k3s-<time>.db.gz && gunzip -c k3s_k3s-<time>.db.gz > state.db
   sqlite3 state.db "PRAGMA integrity_check; SELECT count(*) FROM kine WHERE name LIKE '/bootstrap/%';"   # ok, 1
   head -c 5 postgres_gamemesh-<time>.dump; echo                                                          # PGDMP
   ```
4. **The datastore and the token, then the API server:**
   ```bash
   D=/var/lib/rancher/k3s/server
   sudo install -d -m 700 $D/db
   sudo install -m 600 state.db $D/db/state.db
   sudo install -m 600 token $D/token
   sudo systemctl start k3s
   sudo k3s secrets-encrypt status            # Enabled; All hashes match
   sudo k3s kubectl -n gamemesh get secret gamemesh-secrets
   ```
   A wrong token stops k3s with `bootstrap data already found and encrypted
   with different token`, and that failed start still changes the copy (in the
   drill: a new hash, an 11 MB `-wal`, and new `tls/`, `cred/`, `etc/`). Before
   trying again: `sudo systemctl stop k3s`, remove `$D/db`, `$D/tls`, `$D/cred`,
   `$D/etc` and `$D/token`, and install a fresh `state.db`.
   The API now shows the old server's pods as Running; none is.
5. **Hold the application, and give the volumes their directories.** The
   player service creates the tables when it starts (AutoMigrate), and empty
   tables would stop the dump. The volumes come back as objects whose
   directories are not on the new disk; without them their pods wait in
   ContainerCreating (`path ... does not exist`). The force delete removes the
   old server's pod records, which the kubelet would otherwise start. (In the
   drill the directories came after the agent's start, to pods already
   waiting; here they come first.)
   ```bash
   k=(sudo k3s kubectl)
   app=(gateway leaderboard matchmaking outbox-relay player presence websocket)
   "${k[@]}" -n gamemesh scale deploy "${app[@]}" --replicas=0
   for d in "${app[@]}"; do "${k[@]}" -n gamemesh delete pods -l app=$d --force --grace-period=0; done
   for p in $("${k[@]}" get pv -o jsonpath='{.items[*].spec.local.path}'); do sudo mkdir -m 0777 -p "$p"; done
   ```
6. **The agent, then the database, then the application** (the same shell
   as step 5, for `k` and `app`):
   ```bash
   sudo rm /etc/rancher/k3s/config.yaml.d/90-restore.yaml
   sudo systemctl restart k3s
   until "${k[@]}" -n gamemesh exec deploy/postgres -- pg_isready -q; do sleep 5; done
   "${k[@]}" -n gamemesh exec -i deploy/postgres -- sh -c \
     'pg_restore --no-owner --no-privileges --exit-on-error -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
     < postgres_gamemesh-<time>.dump
   "${k[@]}" -n gamemesh scale deploy "${app[@]}" --replicas=1
   "${k[@]}" -n gamemesh rollout status deploy --timeout=300s
   shred -u ~/restore/*; rmdir ~/restore
   ```
7. **The rest of the rebuild:** steps 5 and 7-9 (kubeconfig, GitHub, DNS,
   deploy), and the k3s backup's units on the host ("The cluster's own
   state", step 4).

A rehearsal (a VM without the public name) restores production's issuers and
backup credentials with everything else. There, leave the issuer files out,
and in step 5 also stop what would reach out:
`"${k[@]}" -n gamemesh patch cronjob postgres-backup -p '{"spec":{"suspend":true}}'`
and `"${k[@]}" -n cert-manager scale deploy --all --replicas=0`. k3s does not
delete the restored issuers for their missing files.

**What does not come back:**

- **Sessions.** Redis and NATS start empty, so every player logs in again: a
  token the old server issued still has a valid signature but is refused
  (`token revoked`). Presence and the leaderboard start empty too.
- **Metrics.** Prometheus's and Grafana's volumes start empty; Grafana's
  dashboards come from the repo.
- **The last hours.** Whatever was written after the backups: up to a day
  (Postgres at 02:15, k3s at 02:30 UTC).

**How long it took:** 38 minutes from the start of the download to a working
login, about 25 of them spent on the two problems above (the cut transfer,
the volumes' directories). The steps themselves: download and decrypt about 4
minutes, the API server 7 s, the dump 1 s, the application 23 s; Postgres was
ready 74 s after its directory existed. The VPS, DNS and GitHub are not in
that figure.

### When an outside alert fires

Prometheus and Grafana run on the server; if the server or its network goes,
so do they. Two services outside watch it and send e-mail to
`ops@gamemesh.app` (the ops accounts, each with its own second factor):

| Service | Watches | Alerts when |
|---|---|---|
| Better Stack (free plan) | `https://api.gamemesh.app/health`: status 200 and `"status":"ok"` in the body | a check fails; checks every 3 minutes, no confirmation period |
| | `https://api.gamemesh.app/api/v1/leaderboard`: status 200 | the same |
| | `https://status.gamemesh.app/`: status 200 | the same |
| Healthchecks.io | the two backups and the daily certificate check ("The dead man's switch") | a run reports a failure, or its ping is overdue |

There is no phone notification: an alert at night is read in the morning.
Better Stack closes an incident by itself once the address has answered for
3 minutes, and sends a second e-mail.

Measured on 2026-11-10 with the gateway scaled to 0 for 40 seconds: the
e-mails for `/health` and the leaderboard route left 13 and 41 seconds after
the scale command, the "resolved" e-mails about 3.5 minutes after the first
good answer, and the status page stayed up. The interval is what is
promised: an outage shorter than 3 minutes can pass unseen.

**`/health` or the leaderboard route is down.** Ask from outside first; the
answer says where to look:

```bash
curl -si https://api.gamemesh.app/health | head -12
```

- **`503` with `no available server`:** Traefik is up and has no gateway pod
  to send to.
  ```bash
  kprod -n gamemesh get deploy/gateway
  kprod -n gamemesh get pods -l app=gateway
  kprod -n gamemesh describe pods -l app=gateway      # the events at the end
  kprod -n gamemesh logs deploy/gateway --tail=50
  ```
  A deployment at `0/0` was scaled down (`kprod -n gamemesh scale
  deploy/gateway --replicas=1`); a pod that does not become ready is "A pod
  does not come up" under "Troubleshooting"; a release that broke it is
  "Rolling back".
- **Only the leaderboard route fails** and `/health` answers: the gateway is
  fine and what it calls is not. `kprod -n gamemesh get pods`, then the
  leaderboard pod's log.
- **No answer at all, or a timeout:** the node or its network. `ssh` over the
  tailnet, then `sudo systemctl status k3s` and `kprod get nodes`; if `ssh`
  does not answer either, the provider's console. A server that is gone is
  "Rebuilding the server from nothing" and "Restoring after losing the
  server".
- **A certificate error:** "The certificate is not renewed" under
  "Troubleshooting".

**The status page is down** and the API is not: Grafana.
`kprod -n monitoring get pods`, then "Status page".

**`cert-check` is down.** The e-mail says which kind:

- **"received a failure signal":** a certificate has fewer than 21 days left
  or does not verify for its name. The run's log names the host and the days:
  ```bash
  gh run list --workflow cert-check.yml --limit 3
  gh run view <run id> --log | grep -E 'days left|no certificate|pinged'
  ```
  Then "The certificate is not renewed".
- **the ping is overdue:** the workflow did not run for a day and a half.
  GitHub turns off scheduled workflows after 60 days without activity in the
  repository; `gh workflow list` shows it, `gh workflow enable
  cert-check.yml` turns it on again, and `gh workflow run cert-check.yml`
  runs it now. Its runs start hours after 06:00 UTC; that alone is not a
  failure.

**A backup's check is down:** "The dead man's switch" under "Backups".

Setting the monitors up again, on a new account: the three addresses above,
for `/health` the "keyword" type with `"status":"ok"`, the others "status
200"; frequency 3 minutes, confirmation "immediate start", recovery 3
minutes; e-mail to `ops@gamemesh.app`. Certificate expiry is a paid feature
there, which is why `cert-check` exists.

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
  or the smoke test fails: `/health` must report the commit,
  `/api/v1/leaderboard` must answer 200 and a join on `/ws` must be answered),
  the job redeploys the commit that ran
  before, with that commit's own manifests and images, and ends red. Its log
  says `rolled back: <previous> runs again`. Tested twice with a deliberately
  broken release:
  - **A release that never becomes ready** (#83): the rollout hit its deadline,
    the old gateway pod kept serving and never restarted.
  - **A release that looks healthy and is not** (#135, 2026-11-14): every pod
    ready, `/health` and the leaderboard answering, and `/ws` refusing every
    login with 401, because the production overlay gave the websocket service
    another token issuer. Only the join on `/ws` can see that. It failed five
    times in 28 s (`upgrade at wss://api.gamemesh.app: HTTP/1.1 401
    Unauthorized instead of 101`), and the previous commit was back 34 s after
    the fifth. In UTC: merged 18:04:23, deploy started 18:11:46, broken
    release rolled out 18:12:31, HTTP checks passed 18:12:33, first failed
    join 18:12:35, deploy failed 18:13:04, rollback rolled out 18:13:33, join
    answered again 18:13:38. `/ws` refused logins for about a minute;
    `/health`, asked every 20 s from outside, answered each time. Here the
    broken pods were ready and took the traffic, so unlike the first drill
    all seven services' pods were replaced twice, and with them every open
    WebSocket connection was closed twice.
- **By hand.** `Actions -> deploy -> Run workflow` (branch `main`) with the full
  SHA of the commit to run. It must be on `main`; its images were published by
  that commit's CI run. Commits from `7b36b40` (#76) on can be deployed this
  way; older overlays still contain the namespace, which CI may not write.
- Which SHA: each deploy log starts with `running before: <sha>`;
  `curl https://api.gamemesh.app/health` shows what runs now; `git log
  --first-parent main` lists the merges.
- What it does not undo: cluster-level manifests (they are not in CI's hands)
  and data. A release that changed the database schema needs its own way back.
- **Redis 8 back to 7.4** (any commit before the move to Redis 8.8). Redis 7.4
  reads 8.8's files only until 8.8 rewrites its append-only file: until then
  the file's base stays in 7.4's format (`REDIS0012`). After a rewrite, which
  8.8 does on its own once the file has grown by 64 MB, the base is
  `REDIS0014` and 7.4 stops with `Can't handle RDB format version 14`. Redis then
  stays down, and with it every request that checks a session. The way out is
  to drop Redis's data, which is sessions, presence and the leaderboard
  (every player logs in again; nothing there is backed up anyway):
  `kprod -n gamemesh delete pvc data-redis-0 --wait=false` then
  `kprod -n gamemesh delete pod redis-0`, and the StatefulSet starts it on a
  new, empty volume. Measured with containers (back to 7.4 works before a
  rewrite, fails after it, works again on an empty volume); the two `delete`
  commands have not been tried on a cluster.

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
(at two thirds of 90 days); the daily `cert-check` workflow alerts when fewer
than 21 days are left ("When an outside alert fires").

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
