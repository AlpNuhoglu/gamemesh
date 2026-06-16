# Observability: Distributed Tracing

GameMesh is instrumented with **OpenTelemetry** and ships traces to **Jaeger**
through an **OpenTelemetry Collector**. This document covers the architecture,
how traces flow, local setup, the env reference, example trace scenarios, and
troubleshooting.

Logging (zap) and metrics (Prometheus + Grafana) are unchanged and still
first-class; tracing is purely additive. Logs are now **correlated** with traces
via `trace_id`/`span_id` fields.

---

## Architecture

```
                         ┌─────────────────────────────────────────────┐
   client                │                  GameMesh                    │
     │                   │                                              │
     │  HTTP (W3C         │   ┌──────────┐  HTTP    ┌──────────┐        │
     │  traceparent)      │   │ gateway  │─────────▶│  player  │──▶ Postgres (GORM)
     ▼                   ─┼──▶│ (otelgin │  otelhttp │ (otelgin │──▶ Redis (sessions)
   POST /api/v1/...       │   │  + proxy │           │ + gorm/  │        │
                          │   │transport)│           │ redisotel)        │
                          │   └──────────┘           └──────────┘        │
                          │        │                                     │
                          │        ├──────────▶ matchmaking ──▶ Redis (queue/rooms)
                          │        └──────────▶ leaderboard ──▶ Redis (sorted set)
                          │                          │                   │
                          │            Redis Pub/Sub │ (traceparent in   │
                          │            events.Carrier▼  event envelope)  │
                          │                     websocket ──▶ WS clients │
                          └─────────────────────────┬───────────────────┘
                                                     │ OTLP/gRPC :4317
                                                     ▼
                                          ┌─────────────────────┐
                                          │  OTel Collector      │  batch + route
                                          │  (otlp → jaeger)     │
                                          └──────────┬──────────┘
                                                     │ OTLP/gRPC
                                                     ▼
                                          ┌─────────────────────┐
                                          │  Jaeger (UI :16686)  │
                                          └─────────────────────┘
```

### Instrumentation map

| Boundary / component        | Instrumentation                                            | Where |
|-----------------------------|-----------------------------------------------------------|-------|
| Incoming HTTP (all services)| `otelgin.Middleware` — continues/creates server span      | `pkg/server/server.go` |
| Gateway → service HTTP      | `otelhttp.NewTransport` on the reverse proxy              | `internal/gateway/proxy.go` |
| Trace ↔ log correlation     | `tracing.LogFields` injects trace_id/span_id              | `pkg/middleware/middleware.go` |
| PostgreSQL (GORM)           | uptrace `otelgorm` plugin (driver-agnostic)              | `pkg/tracing/instrument.go`, `cmd/player` |
| Redis (queue/leaderboard/session/pubsub) | `redisotel.InstrumentTracing` hook          | `pkg/tracing/instrument.go`, all Redis `main.go` |
| Matchmaking workflow        | manual spans: JoinQueue, tick, evict_stale, snapshot, pair, create_room | `internal/matchmaking/service.go` |
| Event publish               | producer span + W3C inject into `events.Carrier`          | `pkg/events/redis.go` |
| Event receive (WS dispatch) | consumer span via `events.ReceiveSpan` (extract carrier)  | `internal/wsgateway/bridge.go` |
| WebSocket connect           | `ws.connect` span at upgrade (pumps NOT traced)           | `internal/wsgateway/handler.go` |

### Design decisions

- **Shared `pkg/tracing`.** One `Init` builds the SDK, installs the global tracer
  provider and the W3C + Baggage propagator, and returns a shutdown func — the
  same centralization pattern as `pkg/server`/`pkg/logger`/`pkg/metrics`. Library
  instrumentations read the global propagator, so HTTP propagation is automatic.
- **Disableable with zero branching.** When `OTEL_ENABLED=false` (or no endpoint),
  `Init` installs a **no-op** tracer provider. All `tracing.Tracer().Start(...)`
  calls then return non-recording spans — effectively free, no `if enabled`
  scattered through the code.
- **Async propagation through Redis Pub/Sub.** The `events.Event` envelope carries
  a `Carrier map[string]string` populated purely via the OTel propagator
  (`Inject`/`Extract`). This is **transport-agnostic**: a future Kafka/NATS
  Publisher reuses the same field and calls — the event contract does not change.
- **Background tick = its own root trace.** Each matchmaking tick starts a fresh
  root span, so Jaeger shows one trace per tick instead of one unbounded trace
  for the whole loop.
- **Configurable sampling from day one.** `OTEL_TRACES_SAMPLER` /
  `OTEL_TRACES_SAMPLER_ARG` map to SDK samplers; dev defaults to always-on,
  production can dial down via env with no code change.
- **Low-cardinality attributes only.** No player_id / username / JWT / session /
  room_id on spans. We record aggregates: rank buckets, ticket/pair counts,
  recipient counts, event type, command type. Library instrumentations use route
  templates and command names (already low-cardinality).
- **Collector in the path.** Batching/retry plus a single place to re-route
  telemetry (swap Jaeger for Tempo/Datadog by editing `config/otel/collector.yaml`,
  no service rebuilds).

---

## Local setup

```bash
# Optional: copy env defaults (the stack also boots with no .env).
cp .env.example .env

# Start everything, including jaeger + otel-collector.
docker compose up --build

# Open the Jaeger UI.
open http://localhost:16686
```

Generate some traffic (register, login, join queue, submit a score), then in the
Jaeger UI pick a service from the **Service** dropdown (`gateway`, `player`,
`matchmaking`, `leaderboard`, `websocket`) and click **Find Traces**.

### Disabling tracing

```bash
OTEL_ENABLED=false docker compose up --build   # whole stack
# or per service / when running a single binary locally:
OTEL_ENABLED=false go run ./cmd/player
```

With tracing disabled, no exporter is dialed and spans are non-recording. You'll
see a `tracing disabled` log line at startup.

---

## Environment reference

| Variable | Default | Meaning |
|----------|---------|---------|
| `OTEL_ENABLED` | `true` | Master switch. `false` installs a no-op provider. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` (compose: `otel-collector:4317`) | OTLP/gRPC target. |
| `OTEL_SERVICE_NAME` | the service's own name | `service.name` resource attribute. |
| `OTEL_TRACES_SAMPLER` | `parentbased_always_on` | `always_on` \| `always_off` \| `traceidratio` \| `parentbased_always_on` \| `parentbased_traceidratio`. |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` | Ratio (0.0–1.0) for the `*ratio` samplers. |
| `SERVICE_VERSION` | `dev` | `service.version` resource attribute. |

---

## Example trace scenarios

### 1. Login
```
gateway: POST /api/v1/auth/login        (otelgin server span)
└─ HTTP GET player (otelhttp client span, traceparent injected)
   └─ player: POST /auth/login          (otelgin server span — same trace)
      ├─ gorm: SELECT players ...        (otelgorm span: query, table, duration)
      └─ redis: SET session:<jti>        (redisotel span: db.system=redis)
```

### 2. Queue join
```
gateway: POST /api/v1/matchmaking/queue
└─ matchmaking: POST /queue
   └─ matchmaking.JoinQueue              (attr: rank_bucket)
      └─ redis: ZADD / HSET (TxPipeline) (redisotel spans)
```

### 3. Match creation (async, crosses Redis Pub/Sub)
```
matchmaking.tick                         (root span — one per tick)
├─ matchmaking.evict_stale               (attr: evicted)
├─ matchmaking.snapshot                  (attr: tickets) → redis ZRANGE
├─ matchmaking.pair                      (attr: tickets_in, pairs_out)
└─ matchmaking.create_room               (per matched pair)
   ├─ redis: SET room:<id>
   ├─ redis: ZREM (dequeue)
   └─ events.publish events.matchmaking  (producer span; traceparent → Carrier)
            ╎ (Redis Pub/Sub)
            ▼
   websocket: ws.dispatch                (consumer span — SAME trace via Carrier)
      └─ hub.SendToPlayer × N            (attr: ws.recipients)
```

### 4. Leaderboard update (async)
```
gateway: POST /api/v1/leaderboard/scores
└─ leaderboard: POST /scores
   ├─ redis: ZINCRBY / ZREVRANK          (redisotel spans)
   └─ events.publish events.leaderboard  (producer span; traceparent → Carrier)
            ╎ (Redis Pub/Sub)
            ▼
   websocket: ws.dispatch                (consumer span — broadcast; attr: ws.fanout=broadcast)
```

---

## WebSocket scope (intentional)

Only **connection establishment** (`ws.connect`) and **event dispatch**
(`ws.dispatch`) are traced. The long-lived read/write pumps and individual
WebSocket frames are deliberately **not** traced — per-frame spans on long-lived
connections would grow without bound and drown out useful traces.

---

## Trace propagation guarantees (tested)

Automated tests protect the two propagation boundaries against regressions:

- **HTTP boundary** — `pkg/server/server_test.go::TestHTTPTracePropagation`:
  an `otelhttp` client call is continued by the `otelgin` server middleware; both
  sides share one trace id.
- **Publish → Subscribe boundary** — `pkg/events/trace_propagation_test.go`:
  `Publish` injects the trace into the event Carrier and it survives the Redis
  round-trip; `ReceiveSpan` extracts it and continues the producing trace.

Run them:
```bash
go test ./pkg/server/ ./pkg/events/ -run 'Trace|Propagation|Receive|Publish'
```

---

## Troubleshooting

**No traces in Jaeger at all**
1. `docker compose logs otel-collector` — the `debug` exporter prints a span
   summary when spans arrive. No output ⇒ services aren't exporting.
2. Check `OTEL_ENABLED` is not `false` and `OTEL_EXPORTER_OTLP_ENDPOINT` points at
   `otel-collector:4317` (compose) — look for the `tracing enabled` / `tracing
   disabled` log line at service startup.
3. Confirm the collector is healthy and Jaeger is up (`docker compose ps`).

**Spans appear but the distributed trace is broken (separate traces)**
- HTTP hop: ensure the call goes through the gateway (otelhttp transport) and the
  downstream uses `server.NewEngine` (otelgin).
- Async hop: confirm the event's `carrier` field is present on the wire (a
  consumer that strips/ignores it breaks the link). The propagation tests above
  guard this.

**Logs don't show trace_id**
- `trace_id`/`span_id` are only attached when a recording span is active. With
  tracing disabled they are intentionally absent.

**Too many traces in production**
- Lower sampling: `OTEL_TRACES_SAMPLER=parentbased_traceidratio` with e.g.
  `OTEL_TRACES_SAMPLER_ARG=0.1`. Consider tail sampling at the Collector.

---

## Future improvements

- **Logs/metrics signals via OTel** in addition to traces (full correlation in
  Grafana via exemplars).
- **Tail-based sampling** at the Collector for production (keep errored/slow
  traces, drop the rest).
- **Span links** for fan-out once events have multiple distinct consumers.
- **Persistent Jaeger storage** (Badger/Elasticsearch) instead of in-memory
  all-in-one.
- **K8s wiring**: expose the same `OTEL_*` env in the manifests under
  `deployments/k8s` and run the Collector as a DaemonSet/sidecar.
