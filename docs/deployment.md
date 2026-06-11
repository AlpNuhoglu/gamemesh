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

## 2. Kubernetes on Minikube

### Prerequisites
- minikube ≥ 1.33, kubectl ≥ 1.30

### Steps

```bash
# 1. Start a cluster with enough headroom
minikube start --cpus=4 --memory=6g
minikube addons enable ingress

# 2. Build the five images directly into Minikube's Docker daemon
#    (avoids pushing to a registry; manifests use imagePullPolicy: IfNotPresent)
make k8s-build
# equivalent to:
#   eval $(minikube docker-env)
#   docker build -f deployments/docker/Dockerfile.gateway -t gamemesh/gateway:latest .
#   ... (player, matchmaking, leaderboard, websocket)

# 3. Deploy everything (namespace, secrets, config, data stores, services,
#    ingress, monitoring — files are numbered in apply order)
kubectl apply -f deployments/k8s/

# 4. Watch it come up
kubectl -n gamemesh get pods -w
```

Expected:

```
NAME                           READY   STATUS    RESTARTS
gateway-...                    1/1     Running   0
gateway-...                    1/1     Running   0
grafana-...                    1/1     Running   0
leaderboard-...                1/1     Running   0
leaderboard-...                1/1     Running   0
matchmaking-...                1/1     Running   0
player-...                     1/1     Running   0
player-...                     1/1     Running   0
postgres-...                   1/1     Running   0
prometheus-...                 1/1     Running   0
redis-...                      1/1     Running   0
websocket-...                  1/1     Running   0
websocket-...                  1/1     Running   0
```

### Accessing the stack

Port-forward (simplest):

```bash
kubectl -n gamemesh port-forward svc/gateway 8080:8080 &
kubectl -n gamemesh port-forward svc/websocket 8084:8084 &
kubectl -n gamemesh port-forward svc/prometheus 9090:9090 &
kubectl -n gamemesh port-forward svc/grafana 3000:3000 &
curl localhost:8080/healthz
```

Or via ingress:

```bash
echo "$(minikube ip)  gamemesh.local" | sudo tee -a /etc/hosts
curl http://gamemesh.local/api/v1/leaderboard/top/10
# WebSocket: ws://gamemesh.local/ws?token=...
```

### Useful commands

```bash
kubectl -n gamemesh logs deploy/gateway -f            # follow gateway logs
kubectl -n gamemesh logs deploy/matchmaking -f        # watch the match loop
kubectl -n gamemesh describe pod <pod>                # probe/image issues
kubectl -n gamemesh rollout restart deploy/player     # restart after rebuild
kubectl -n gamemesh exec deploy/redis -- redis-cli zcard matchmaking:queue
```

### Production secrets

`01-secrets.yaml` ships dev values for Minikube convenience. In a real
cluster, never commit secrets — create them out of band:

```bash
kubectl -n gamemesh create secret generic gamemesh-secrets \
  --from-literal=JWT_SECRET="$(openssl rand -hex 32)" \
  --from-literal=POSTGRES_USER=gamemesh \
  --from-literal=POSTGRES_PASSWORD="$(openssl rand -hex 16)" \
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
