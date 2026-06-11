# Troubleshooting

## Docker Compose

**A service container restarts in a loop**
`docker compose logs <service>`. Most common: Postgres/Redis not healthy yet
(healthcheck-gated `depends_on` should prevent this — check
`docker compose ps` for unhealthy dependencies).

**`pull access denied` or build fails on `go mod download`**
Ensure you build from the repo root (`docker compose up --build`); the
Dockerfiles expect the build context to be the repository root.

**Postgres schema missing after a previous run**
Init SQL only runs on first boot of the volume:
`docker compose down -v && docker compose up --build`.
(The player service also auto-migrates when `AUTO_MIGRATE=true`, the default.)

**401 from protected endpoints**
Tokens expire after `JWT_EXPIRY` (24h default). Re-login. If you changed
`JWT_SECRET`, all previously issued tokens are invalid — by design.

**429 Too Many Requests during load tests**
Raise the gateway limits: `RATE_LIMIT_RPS=1000 RATE_LIMIT_BURST=2000` in
`.env`, then `docker compose up -d gateway`.

## Kubernetes / Minikube

**`ImagePullBackOff`**
Images weren't built into Minikube's Docker daemon. Run `make k8s-build`
(which wraps `eval $(minikube docker-env)` + builds). Verify with
`minikube image ls | grep gamemesh`.

**Pods `Pending`**
Insufficient cluster resources: `kubectl describe pod <pod>` → look at
Events. Start Minikube bigger: `minikube start --cpus=4 --memory=6g`.

**Ingress 404 / connection refused**
`minikube addons enable ingress`, wait for the controller pod in
`ingress-nginx` namespace, and confirm the `/etc/hosts` entry points at
`minikube ip` (it changes between `minikube start`s).

**Player pods CrashLoopBackOff with Postgres connection errors**
Postgres may still be initialising on first boot. The readiness probe gates
traffic but not startup order; pods retry and recover. If it persists:
`kubectl -n gamemesh logs deploy/postgres`.

**WebSocket drops after ~60s through ingress**
The provided ingress sets `proxy-read-timeout: 3600`. If you customised the
ingress, restore those annotations; the app side keeps connections alive with
pings every 54s.

## Matchmaking

**Players queue but never match**
- Ranks must be within `MATCH_RANK_WINDOW` (default 100) of an adjacent
  queued player.
- The loop ticks every `MATCH_INTERVAL_SECONDS` (default 5) — wait one tick.
- Inspect the queue directly:
  `redis-cli zrange matchmaking:queue 0 -1 WITHSCORES`.
- Tickets older than `MATCH_MAX_QUEUE_AGE` (default 5m) are evicted as
  presumed disconnects.

**MatchFound never reaches the client**
The WS connection must be established *before* the match tick (events are
fire-and-forget Pub/Sub). Confirm the client is connected
(`gamemesh_websocket_active_connections` in Prometheus) and that the
matchmaking logs show `match created`.

## Tests

**Integration tests fail with `Cannot connect to the Docker daemon`**
Testcontainers needs a running Docker daemon. On CI use a runner with Docker
(GitHub's `ubuntu-latest` works out of the box).

**Integration tests are slow the first run**
They pull `postgres:16-alpine` / `redis:7-alpine` once; subsequent runs reuse
the local images.

## Observability

**Service missing from Prometheus targets (compose)**
Targets are static in `config/prometheus/prometheus.yml` — Prometheus →
Status → Targets shows scrape errors per job.

**Service missing from Prometheus (K8s)**
Scraping is annotation-driven; the pod template needs
`prometheus.io/scrape: "true"` and the right `prometheus.io/port`.

**Grafana panels empty**
Generate traffic (run the smoke test or a k6 script). Several panels are
rates over 1–5m windows and need a couple of minutes of data.
