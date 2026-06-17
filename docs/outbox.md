# Transactional Outbox Pattern

GameMesh writes business state to PostgreSQL and broadcasts domain events over
NATS JetStream. This document explains the **dual-write problem** that arises
when those two writes are independent, and the **transactional outbox** that
eliminates it — guaranteeing that a committed database transaction can never
lose its corresponding event.

> **Companion docs:** [messaging.md](messaging.md) (the NATS JetStream
> transport) and [observability.md](observability.md) (tracing/metrics). This
> doc assumes the event contract and `events.Publisher`/`Subscriber`
> abstractions described there.

---

## 1. The dual-write problem

A service that both **persists state** and **publishes an event** is performing
two writes to two systems that do not share a transaction:

```
BEGIN
  INSERT players ...
COMMIT                ← Postgres durably committed

publisher.Publish(MatchFound)   ← separate write to NATS
```

Any of these now loses the event while the state is already durable:

- the process **crashes** between `COMMIT` and `Publish`;
- NATS is **briefly unavailable** when `Publish` runs;
- the publish call **times out** or the network drops.

The database says the player exists; no `PlayerRegistered` event was ever sent.
Downstream consumers (analytics, notifications, projections) silently diverge
from the source of truth. Reversing the order is no better — publishing first
then committing can emit an event for a transaction that later rolls back, which
is a *phantom* event. **There is no ordering of two independent writes that is
safe.**

---

## 2. Why the outbox pattern exists

The outbox pattern collapses the two writes into **one transaction against one
store**. Instead of publishing to NATS inline, the producer inserts the event
into an `outbox_events` table **in the same transaction** as the business write:

```
BEGIN
  INSERT players ...
  INSERT player_stats ...
  INSERT outbox_events (PlayerRegistered)   ← same transaction
COMMIT
```

Now the event and the state commit **atomically**: either both are durable or
neither is. A separate **relay** process then reads committed outbox rows and
publishes them to NATS, retrying until each succeeds. The database becomes the
single source of truth for "an event must be sent."

```
HTTP Register
     │
     ▼
player.Service.Register ──(one Postgres tx)── players + player_stats + outbox_events
                                                          │ COMMIT
                                                          ▼
                          outbox-relay  ──poll PENDING──► events.Publisher ──► NATS JetStream ──► consumers
                                        ──mark PUBLISHED─┘
```

### Where it applies in GameMesh (and where it does not)

A real outbox requires the business write and the outbox row to commit in **one
transaction in one store**. In GameMesh only the **player service** is backed by
PostgreSQL, so it is the only place a *genuine* transactional outbox is
achievable. It now emits `PlayerRegistered` and `PlayerUpdated` through the
outbox.

The **matchmaking** and **leaderboard** services keep their state in **Redis**,
which cannot share an ACID transaction with a Postgres outbox. Their
`MatchFound` / `LeaderboardUpdated` events are therefore **out of scope** for the
outbox and continue to publish directly to NATS. Bringing them under the outbox
would require relocating their authoritative state into Postgres — a larger
change deliberately not undertaken here. This limitation is called out honestly
rather than papered over with a non-atomic "outbox" that wouldn't actually
guarantee anything.

---

## 3. Data model

`migrations/0003_create_outbox_events.up.sql` (also folded into
`scripts/db/schema.sql` for the compose first-boot path, and registered with
GORM `AutoMigrate` via `outbox.OutboxEvent`):

| Column          | Type          | Purpose                                                        |
| --------------- | ------------- | -------------------------------------------------------------- |
| `id`            | `UUID` PK     | **Equals `events.Event.ID`** — the stable consumer dedup key.  |
| `event_type`    | `TEXT`        | e.g. `PlayerRegistered`.                                       |
| `topic`         | `TEXT`        | e.g. `events.player` — the publish destination.                |
| `payload`       | `JSONB`       | The domain payload (`events.Event.Payload`).                   |
| `carrier`       | `JSONB`       | W3C trace headers captured at write time (trace continuity).   |
| `status`        | `TEXT`        | `PENDING` → `PUBLISHED`.                                        |
| `created_at`    | `TIMESTAMPTZ` | Poll ordering (oldest first) and audit.                        |
| `published_at`  | `TIMESTAMPTZ` | `NULL` until relayed; audit trail.                             |
| `attempt_count` | `INTEGER`     | Bumped on each failed publish; surfaces poison rows.           |

```sql
CREATE INDEX idx_outbox_pending
    ON outbox_events (created_at)
    WHERE status = 'PENDING';
```

### Schema rationale

- **`id = Event.ID`.** Reusing the event UUID as the primary key means the same
  identity flows DB → NATS → consumer. A consumer can dedup on `Event.ID` with
  zero extra columns and no coordination.
- **`carrier JSONB`.** The trace context is captured *inside the originating
  request's span* at insert time and replayed by the relay, so the eventual NATS
  publish links back to the HTTP request even though it happens later, in another
  process. Without this, the trace would break at the async boundary.
- **Partial index on `PENDING`.** The relay's only hot query is "oldest
  unpublished rows." A partial index stays small as `PUBLISHED` rows accumulate,
  keeping the poll `O(batch)` regardless of total table size.
- **Two states, no `FAILED`.** See §6.

---

## 4. Relay architecture

The relay is a **dedicated process** (`cmd/outbox-relay`, its own container in
`docker-compose.yml`), not a goroutine inside the player service.

**Why dedicated:** it scales and restarts independently of the player API;
`FOR UPDATE SKIP LOCKED` lets multiple replicas run safely; relay load never
competes with request handling; and the player service needs **no NATS
connection at all** — it only writes rows. The cost is one more container, which
is the standard production trade-off.

Each poll cycle (`internal/outbox/store.go: RunBatch`) runs inside **one
transaction**:

1. **Claim** up to `OUTBOX_BATCH_SIZE` of the oldest `PENDING` rows with
   `SELECT ... FOR UPDATE SKIP LOCKED`. `SKIP LOCKED` means concurrent relay
   replicas never grab the same rows — horizontal scaling is free.
2. **Publish** the batch across a **bounded worker pool** of `OUTBOX_WORKERS`
   (`internal/outbox/relay.go: publishAll`) — never one goroutine per event.
3. **Mark** successfully published rows `PUBLISHED` (stamping `published_at`) and
   **bump `attempt_count`** on the failures, which stay `PENDING`.
4. **Commit.** The rows stayed locked for the whole cycle, so no other replica
   touched them.

When a poll returns a full batch the relay loops immediately (draining a
backlog); otherwise it waits `OUTBOX_POLL_INTERVAL`. Shutdown is driven by
`server.ShutdownContext()` (SIGINT/SIGTERM): the loop stops polling and the
in-flight batch's transaction completes or rolls back cleanly.

### Configuration

| Env var                | Default | Meaning                              |
| ---------------------- | ------- | ------------------------------------ |
| `OUTBOX_ENABLED`       | `true`  | Gate the relay loop.                 |
| `OUTBOX_BATCH_SIZE`    | `100`   | Rows claimed per poll.               |
| `OUTBOX_POLL_INTERVAL` | `1s`    | Delay between polls when idle.       |
| `OUTBOX_WORKERS`       | `4`     | Bounded publish concurrency / batch. |

The player service always **writes** to the outbox regardless of
`OUTBOX_ENABLED`; the flag only governs the relay.

---

## 5. NATS interaction

The relay reuses the existing `events.NATSBus` (`Transport: "nats"`) unchanged.
For each row it reconstructs an `events.Event` from the stored columns —
including the carrier — and calls `bus.Publish(ctx, topic, e)`. From NATS's
perspective this is an ordinary publish to the `events.player` stream; the
`PLAYER` stream is created idempotently on boot like the others (see
[messaging.md](messaging.md)). Consumers subscribe exactly as they do for any
other event. The outbox is invisible past the publish.

---

## 6. Event lifecycle and failure scenarios

```
        insert (in business tx)
PENDING ───────────────────────► (relay publishes) ──success──► PUBLISHED
   ▲                                     │
   └───────────── failure ───────────────┘  (attempt_count++ , stays PENDING)
```

**Only two states.** A failed publish is simply a `PENDING` row that is retried
on the next poll, so a transient NATS outage **self-heals** with no operator
action and nothing to reconcile. There is intentionally **no `FAILED` state**:
it would add a manual recovery step for a condition that resolves itself.
`attempt_count` is enough to alert on a genuinely poisoned row (e.g. one that
keeps failing) without a state machine.

| Failure                                          | Outcome                                                                 |
| ------------------------------------------------ | ----------------------------------------------------------------------- |
| Crash between `COMMIT` and any publish           | Row is durable & `PENDING`; relay publishes it on next poll. **No loss.** |
| NATS down when relay polls                       | `Publish` fails; row stays `PENDING`, `attempt_count++`; retried later. |
| Crash **after** NATS publish, **before** mark    | Row stays `PENDING`; relay republishes → **duplicate** (see §7).        |
| Business `INSERT` fails (e.g. duplicate username)| Whole tx rolls back; **no orphan outbox row**.                          |
| Two relay replicas poll simultaneously           | `SKIP LOCKED` hands each row to exactly one replica.                    |

---

## 7. Idempotency

**Publishing is at-least-once, not exactly-once.** Exactly-once would require
distributed consensus (2PC) between Postgres and NATS, which neither supports
cheaply and which the outbox deliberately avoids. The crash window in §6 (publish
succeeded, mark didn't) means the relay can republish a row — so **duplicates are
possible by design**. Losing an event is unacceptable; a duplicate is tolerable,
because **consumers are idempotent**.

### Why this isn't a new burden

Every consumer already runs under JetStream's at-least-once delivery (durable
consumers redeliver on `AckWait` expiry). The outbox adds another duplicate
source but no new *requirement* — idempotent consumption was always mandatory.

### Consumer audit

- **`MatchFound`** (matchmaking → WS): the WS bridge sends the message to the
  matched players' connections. Re-sending the same notification is harmless —
  the in-memory hub operations are naturally idempotent.
- **`LeaderboardUpdated`** (leaderboard → WS): broadcast to all clients. A
  repeated broadcast carries the same score/rank snapshot; harmless.
- **`PlayerRegistered` / `PlayerUpdated`** (new, via the outbox): no consumer
  exists yet — the relay makes them durably available for future projections.
  **Prescribed dedup strategy:** key on `Event.ID` (== outbox `id`). A consumer
  that records processed IDs (or upserts by a natural key) collapses duplicates.

We deliberately **do not** build a dedup table or inbox now — that would be
over-engineering for events with no consumer. The stable `Event.ID` is the seam
that makes dedup trivial when a consumer arrives.

---

## 8. Distributed tracing

The trace must stay unbroken across the async hand-off: HTTP request → outbox
insert → (later, another process) relay publish → NATS → consumer.

Spans created (all on the shared `tracing.Tracer()`):

| Span                   | Where                          | Notes                                  |
| ---------------------- | ------------------------------ | -------------------------------------- |
| `outbox.insert <topic>`| `OutboxPublisher.Publish`      | Inside the business transaction.       |
| `outbox.poll`          | `Relay.processBatch`           | One per poll cycle.                    |
| `outbox.publish <topic>`| `Relay.publishOne`            | Continues the originating trace.       |
| `outbox.mark_published`| `Store.RunBatch`               | The status update.                     |

**How continuity is preserved:** `OutboxPublisher.Publish` injects the current
trace context into `Event.Carrier` and stores it in the `carrier` column — at
insert time, so it captures the *request's* span. The relay reads `carrier` back,
`Extract`s it into the context before starting `outbox.publish`, and the existing
`NATSBus.Publish` producer span (and the downstream consumer span) therefore
share the **same trace ID** as the original HTTP request. This is verified by
`TestRelayPropagatesTrace` in `internal/outbox/relay_test.go`.

---

## 9. Metrics

Prometheus instruments (Prometheus scrapes the relay at `outbox-relay:8085`),
following the existing `gamemesh_*` convention with a `service` const label:

| Metric                                   | Type      | Meaning                                  |
| ---------------------------------------- | --------- | ---------------------------------------- |
| `gamemesh_outbox_events_pending`         | Gauge     | Current `PENDING` backlog (per poll).    |
| `gamemesh_outbox_events_published_total` | Counter   | Rows successfully relayed.               |
| `gamemesh_outbox_publish_failures_total` | Counter   | Publish attempts that failed (retried).  |
| `gamemesh_outbox_publish_duration_seconds`| Histogram| Per-row publish latency.                 |

**Operational signals:** a steadily rising `pending` gauge means the relay is
falling behind or NATS is unreachable; a rising `publish_failures_total` with a
flat `published_total` means NATS is down (rows are safe, just delayed).

---

## 10. Operational considerations

- **Retention.** `PUBLISHED` rows are kept for audit/debugging. Add a periodic
  `DELETE FROM outbox_events WHERE status='PUBLISHED' AND published_at < now() - interval '7 days'`
  job in production (not implemented here) to bound table growth. The partial
  index keeps the poll fast regardless, so this is housekeeping, not correctness.
- **Scaling the relay.** Raise replicas freely — `FOR UPDATE SKIP LOCKED`
  partitions work across them. Tune `OUTBOX_WORKERS`/`OUTBOX_BATCH_SIZE` for
  throughput.
- **Ordering.** The relay processes oldest-first but does not guarantee strict
  global ordering under concurrency. Consumers must not assume cross-event order.
- **Kubernetes.** The compose deployment runs the relay; the k8s manifests under
  `deployments/k8s/` are not updated in this change and would need an analogous
  Deployment to run the relay in-cluster.

---

## 11. Verification

```bash
go build ./...   # compiles
go vet ./...     # clean
go test ./...    # unit tests (incl. relay scenarios) pass
```

Unit tests (`internal/outbox/relay_test.go`, no DB required): relay publishes a
pending event, retries on publish failure, is duplicate-safe across the crash
window, and propagates the trace. Service-level
(`internal/player/service_test.go`): `Register` emits a `PlayerRegistered` event
through the outbox path.

Integration tests (`tests/integration/outbox_test.go`, Testcontainers Postgres,
run with `-tags integration`): the outbox row is written **with** the business
rows; a failed business transaction leaves **no orphan** outbox row (atomicity);
and `Store.RunBatch` claims, publishes and marks rows `PUBLISHED`.

End-to-end (manual): `docker compose up`, `POST /auth/register`, then confirm in
Jaeger that one trace spans `player.register` → `outbox.insert` → (relay)
`outbox.publish` → NATS, and that `gamemesh_outbox_events_published_total`
increments in Prometheus.
