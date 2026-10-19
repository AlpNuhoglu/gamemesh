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

### Production secrets

The local overlay generates `gamemesh-secrets` from dev values
(`overlays/local/secrets.env`). The production overlay generates no Secret:
the cluster must hold one, created out of band and never committed:

```bash
kubectl -n gamemesh create secret generic gamemesh-secrets \
  --from-literal=JWT_SECRET="$(openssl rand -hex 32)" \
  --from-literal=POSTGRES_USER=gamemesh \
  --from-literal=POSTGRES_PASSWORD="$(openssl rand -hex 32)" \
  --from-literal=POSTGRES_DB=gamemesh
```

(or External Secrets Operator / Sealed Secrets / a cloud secret manager).

## 3. Grafana dashboard import (non-provisioned Grafana)

1. Grafana → **Dashboards → New → Import**
2. Upload `config/grafana/dashboards/gamemesh.json`
3. Select your Prometheus datasource → **Import**

In the compose stack this happens automatically via provisioning.

## 4. CI/CD

`.github/workflows/ci.yml` runs on every push/PR:

1. **Lint** — golangci-lint
2. **Test** — unit tests with `-race` + coverage artifact
3. **Integration** — Testcontainers against real Postgres/Redis
4. **Build** — all five binaries
5. **Docker** — buildx matrix build of all five images (GHA-cached)
6. **Security** — Trivy fs scan (HIGH/CRITICAL fail the build) + govulncheck
7. **Deploy (example)** — client-side `kubectl apply --dry-run` validation of
   the manifests; swap in a real `kubectl apply` with cluster credentials.
