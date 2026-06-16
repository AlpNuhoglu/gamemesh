# Messaging & Event Transport

GameMesh is event-driven: services communicate asynchronously through a small,
transport-agnostic event contract. This document explains the migration of that
transport from **Redis Pub/Sub** to **NATS JetStream**, the guarantees each
provides, and the path toward a transactional Outbox.

---

## 1. Why migrate from Redis Pub/Sub

Redis Pub/Sub was the original transport. It is fast and was already in the
stack, but it is **fire-and-forget**:

- A subscriber that is **down or restarting** when a message is published
  **never sees it** — the message is dropped at the broker.
- There is **no acknowledgement** — the publisher cannot know a consumer
  processed the event, and a consumer cannot ask for redelivery on failure.
- There is **no history / replay** — you cannot re-read past events to rebuild
  state or onboard a new consumer.

For realtime hints (a leaderboard tick will repair itself on the next poll) this
is acceptable. For events that drive state and user-visible actions
(`MatchFound` routes players into a game), losing a message is a correctness
problem. NATS JetStream gives us **durability, acknowledgements, replay and
at-least-once delivery** while keeping the same clean abstraction.

> **Redis is not removed.** It remains the system of record for sessions, the
> matchmaking queue, the leaderboard sorted set, and the room cache. JetStream
> replaces **only** the inter-service event transport.

---

## 2. Redis Pub/Sub vs NATS JetStream

| Property              | Redis Pub/Sub                | NATS JetStream                       |
| --------------------- | ---------------------------- | ------------------------------------ |
| Delivery guarantee    | At-most-once                 | **At-least-once**                    |
| Durability            | None (in-memory fan-out)     | **Persisted to a stream** (file)     |
| Consumer offline      | Messages lost                | **Buffered, delivered on reconnect** |
| Acknowledgement       | None                         | **Explicit ACK / NAK / TERM**        |
| Retry on failure      | Not possible                 | **Automatic redelivery (MaxDeliver)**|
| Replay / history      | Not possible                 | **DeliverAll / by time / by seq**    |
| Backpressure          | None (drops)                 | **Flow control (pending limits)**    |
| Outbox compatibility  | Poor (no durable hand-off)   | **Natural durable hand-off point**   |
| Latency               | Sub-millisecond              | Low (single-digit ms, persisted)     |

Core NATS (without JetStream) was rejected because it shares Pub/Sub's
at-most-once / no-replay limitations. JetStream is required for the durability
and Outbox-readiness goals.

---

## 3. Delivery guarantees

The architectural seam is unchanged: services depend only on
[`events.Publisher`](../pkg/events/events.go) and
[`events.Subscriber`](../pkg/events/events.go). Two transports implement them:

- `RedisBus` — at-most-once (unchanged, kept for fallback / comparison).
- `NATSBus` — at-least-once.

### How at-least-once is enforced without changing the interface

The legacy `Subscriber` returns a channel and has **no place to signal
processing success**. Rather than break that interface (and every service using
it), we added an **opt-in** richer contract:

```go
type Handler func(ctx context.Context, e Event) error

type AckSubscriber interface {
    SubscribeAck(ctx context.Context, handler Handler, topics ...string) error
}
```

`NATSBus` implements **both** `Subscriber` (channel, drop-in compatible) and
`AckSubscriber` (handler, true at-least-once). The WS bridge type-asserts:

```go
if ack, ok := sub.(events.AckSubscriber); ok {
    return ack.SubscribeAck(ctx, b.dispatch, topics...) // ACK after success
}
// else: legacy channel path
```

A message is **ACK'd only after the handler returns `nil`**. On error it is
**NAK'd with a delay** and redelivered up to `MaxDeliver` (5). Malformed/poison
messages are **TERM'd** (never redelivered). Because redelivery is possible,
**handlers must be idempotent**.

---

## 4. Replay examples

JetStream persists events, so they can be re-read. Using the `nats` CLI:

```bash
# Inspect a stream and its consumers
nats stream info MATCHMAKING
nats consumer info MATCHMAKING websocket-matchmaking

# Tail everything currently in a stream (no ACK side effects)
nats stream view MATCHMAKING

# Replay the full history into a brand-new ephemeral consumer
nats consumer add MATCHMAKING replay-demo \
  --filter 'events.matchmaking.>' --ephemeral \
  --deliver all --ack none
nats consumer next MATCHMAKING replay-demo --count 100

# Replay only events from the last 10 minutes
nats consumer add MATCHMAKING replay-recent --ephemeral \
  --deliver 'by_start_time' --start-time '10m ago' --ack none
```

In code, replay is simply a consumer created with `DeliverPolicy:
DeliverAllPolicy` — which is exactly what a freshly-started service gets on its
first boot, so a new consumer automatically catches up on history. This is
verified by `TestNATSReplay` in
[`pkg/events/nats_test.go`](../pkg/events/nats_test.go): an event published
**before** any consumer exists is still delivered.

---

## 5. Stream topology

Streams are **per-domain, not a single catch-all**, so each domain's retention,
limits, storage and (future) replication can be tuned independently and one
domain's load never affects another.

| Stream        | Subjects                                          | Retention (MaxAge) | Rationale                                              |
| ------------- | ------------------------------------------------- | ------------------ | ----------------------------------------------------- |
| `MATCHMAKING` | `events.matchmaking`, `events.matchmaking.>`      | 24h                | Match events are valuable to replay for a day.        |
| `LEADERBOARD` | `events.leaderboard`, `events.leaderboard.>`      | 1h                 | Score updates are high-volume and quickly disposable. |

Storage is `FileStorage` (persisted to the `natsdata` volume) with
`LimitsPolicy` retention.

### Subject naming

The publish **topics** keep their existing constant values
(`events.matchmaking`, `events.leaderboard`) so the **event contract is
unchanged**. The actual publish subject appends the event type:

```
events.matchmaking.MatchFound
events.leaderboard.LeaderboardUpdated
```

The stream's hierarchical wildcard (`events.matchmaking.>`) captures these and
leaves room for future subtypes (`events.matchmaking.MatchCancelled`) **without
new streams**, and lets future consumers filter per-type if needed.

---

## 6. Consumer topology

| Consumer (durable name)      | Stream        | Filter                      | Owner        |
| ---------------------------- | ------------- | --------------------------- | ------------ |
| `websocket-matchmaking`      | `MATCHMAKING` | `events.matchmaking.>`      | WS bridge    |
| `websocket-leaderboard`      | `LEADERBOARD` | `events.leaderboard.>`      | WS bridge    |

Each consumer is a **durable pull consumer**:

- **Durable** — named after the consuming service, so restarts resume from the
  last ACK'd position (no replay storm, no loss).
- `AckExplicitPolicy` — manual ACK required.
- `AckWait: 30s` — if not ACK'd in time, redeliver.
- `MaxDeliver: 5` — bounded retries, then stop (future: dead-letter subject).
- `DeliverAllPolicy` — a fresh consumer replays history (catch-up on first boot).

### Concurrency model (no goroutine-per-message)

A goroutine per message is unbounded and risks OOM under burst. Instead each
consumer runs a **bounded worker pool** (`EVENT_WORKERS`, default 8):

```
JetStream pull ──> internal work channel ──> [worker 1 .. worker N] ──> handler ──> ACK/NAK
```

- **Backpressure**: `PullMaxMessages` caps in-flight (unacked) messages; the
  pull side blocks when the pool is saturated, so consumers never outrun
  processing.
- **Graceful shutdown**: on `ctx` cancellation the consume loop stops pulling,
  the work channel is closed, workers drain buffered messages, then the NATS
  connection is `Drain()`ed. In-flight, un-ACK'd messages simply redeliver
  later — safe because handlers are idempotent.
- **Context cancellation** propagates from the service's shutdown context all
  the way into the pool.

---

## 7. Trace propagation across the async boundary

Distributed tracing (OpenTelemetry → OTel Collector → Jaeger) is preserved
**unchanged in mechanism**. The `Event` envelope carries a `Carrier
map[string]string` holding W3C `traceparent`/`tracestate`:

```
matchmaking.tick ──(publish: inject Carrier)──> JetStream ──(consume: extract Carrier)──> ws.dispatch
```

- **Publish** starts a `SpanKindProducer` span and injects the active trace
  context into `Event.Carrier` via the OTel propagator.
- **Consume** extracts the Carrier and starts a `SpanKindConsumer` span
  (`events.consume <topic>`), then the bridge's `ws.dispatch` runs under it.

Because this relies only on the propagator and the Carrier field, it is
**transport-agnostic** — Redis and NATS use the identical inject/extract calls;
only the `messaging.system` span attribute differs (`redis` vs `nats`). A single
`MatchFound` therefore remains **one connected trace** in Jaeger:
`matchmaking.tick → events.publish → events.consume → ws.dispatch`. Verified by
`TestNATSPublishConsumeAck`.

---

## 8. Metrics

Both transports emit the same Prometheus instruments (registered per service in
[`pkg/metrics`](../pkg/metrics/metrics.go)), so dashboards work regardless of
`EVENT_BUS`:

| Metric                                         | Labels          | Meaning                                    |
| ---------------------------------------------- | --------------- | ------------------------------------------ |
| `gamemesh_events_published_total`              | `topic`, `type` | Events published.                          |
| `gamemesh_events_consumed_total`               | `topic`, `type` | Events processed and ACK'd.                |
| `gamemesh_events_failed_total`                 | `topic`, `stage`| Failures by stage (`publish`/`decode`/`handle`). |
| `gamemesh_event_processing_duration_seconds`   | `topic`, `type` | Handler latency histogram.                 |

---

## 9. Configuration

| Variable        | Default              | Purpose                                    |
| --------------- | -------------------- | ------------------------------------------ |
| `EVENT_BUS`     | `redis` (app) / `nats` (compose) | Transport selection: `redis` or `nats`. |
| `NATS_URL`      | `nats://localhost:4222` | NATS endpoint (compose: `nats://nats:4222`). |
| `EVENT_WORKERS` | `8`                  | Bounded worker-pool size per consumer.     |

The transport switch lives in **one place**,
[`events.NewBus`](../pkg/events/factory.go), so adding a future transport
(Kafka, gRPC streaming) touches a single file and **no service code**.

---

## 10. Future Outbox integration strategy

The current design is deliberately **Outbox-ready** but does **not** implement
the Outbox pattern yet. The seam that makes it cheap: producers depend only on
`events.Publisher`.

Today (direct publish):

```
service ──> Publisher.Publish ──> JetStream
```

A dual-write risk exists: the DB commit and the publish are separate operations,
so a crash between them can lose or duplicate an event. The Outbox pattern fixes
this **without touching producer code**:

1. **`OutboxPublisher implements events.Publisher`** — instead of publishing to
   NATS, `Publish` inserts the event row into an `outbox` table **inside the same
   DB transaction** as the business state change. Atomic: either both commit or
   neither does.
2. **A relay process** polls the `outbox` table (or tails the WAL via CDC) and
   calls the real `NATSBus.Publish` for each unsent row, marking it sent on
   success. JetStream's publish ACK confirms durable hand-off; the relay retries
   on failure.

```
service ──> OutboxPublisher.Publish ──> [DB tx: state + outbox row]
                                              │
                            relay (poll/CDC) ─┘──> NATSBus.Publish ──> JetStream
```

Because `OutboxPublisher` satisfies the **same interface**, wiring it is a
factory change (`EVENT_BUS=outbox`) — **zero changes** to matchmaking,
leaderboard, or any business logic. JetStream being durable makes it the natural
sink for the relay, and at-least-once + idempotent handlers already tolerate the
duplicates an Outbox relay can produce.

---

## 11. Verification

```bash
go mod tidy
go build ./...
go test ./...          # includes embedded-JetStream tests in pkg/events
docker compose up      # brings up nats (JetStream), redis, jaeger, etc.
```

Embedded-server tests (no docker needed) in
[`pkg/events/nats_test.go`](../pkg/events/nats_test.go) prove:

- `TestNATSPublishConsumeAck` — publish → consume → ACK with trace continuity.
- `TestNATSRedeliversOnHandlerError` — at-least-once: failed handler → NAK →
  redelivery.
- `TestNATSReplay` — an event published before any consumer existed is still
  delivered (replay).

End-to-end with the stack running: generate a `MatchFound` (queue two players in
matchmaking) and a `LeaderboardUpdated` (submit a score), then confirm the event
reaches the WS client, `nats consumer info` shows it ACK'd, Jaeger shows one
connected trace, and a replay consumer re-reads it.
