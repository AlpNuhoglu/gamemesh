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

## Kubernetes (k3d)

**`ErrImagePull` / `ImagePullBackOff` on gamemesh images**
The local overlay uses `gamemesh/<service>:dev` loaded into the cluster, not a
registry. Run `make k8s-build` (builds every `cmd/` service and runs
`k3d image import`). In the production overlay the tag is `set-by-deploy`
until CI sets the commit's SHA, so a pull failure there means it was applied
by hand.

**Pods `Pending`**
Insufficient resources or an unbound volume: `kubectl describe pod <pod>` →
Events. NATS, Redis and Postgres need the `local-path` StorageClass k3s ships.

**Every request through Traefik returns 404**
Send the Ingress host: `curl -H 'Host: gamemesh.localhost'
http://127.0.0.1:18080/health`. Only `/health`, `/api` and `/ws` are routed.
An Ingress with a class other than `traefik` is ignored without an error
(`kubectl -n gamemesh get ingress` shows no address).

**Pods cannot find `gamemesh-secrets`**
The local overlay generates `gamemesh-secrets-<hash>` and rewrites the
references; the production overlay expects a Secret created in the cluster.
Check with `kubectl kustomize deployments/k8s/overlays/<env> | grep -n
gamemesh-secrets`.

**Player pods restart once on a fresh database**
Two replicas can run GORM's AutoMigrate at the same moment and one fails on
an existing constraint; the restarted pod finds the schema and starts. Waiting
for Postgres itself is retried (`database.Open`), not a crash.

**WebSocket drops through the ingress**
Not yet checked on Traefik (A10 in the deployment plan); the app side pings
every 54s.

## Matchmaking

**Players queue but never match**
- Ranks must be within `MATCH_RANK_WINDOW` (default 100) of an adjacent
  queued player.
- The loop ticks every `MATCH_INTERVAL_SECONDS` (default 5) — wait one tick.
- Inspect the queue directly (the container's redis-cli is already
  authenticated):
  `docker compose exec redis redis-cli zrange matchmaking:queue 0 -1 WITHSCORES`.
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
