# Presence Service

The Presence Service is the foundation of GameMesh's social layer. It maintains a
distributed, self-healing view of **where every player is** — not just a boolean
online flag — and publishes presence transitions as events that friends, parties,
invites, reconnect and notifications build on.

It is a separate microservice (port `8086`). The WebSocket gateway feeds it
connection lifecycle over HTTP; presence ownership lives here, not in the WS
gateway — the two stay decoupled.

- Code: [`internal/presence`](../internal/presence), entrypoint
  [`cmd/presence/main.go`](../cmd/presence/main.go)
- Event contract: [`pkg/events/events.go`](../pkg/events/events.go)
  (`TopicPresence`, `PresenceChangedPayload`)

---

## State machine

```
OFFLINE   – no live connection (a missing presence key IS offline)
ONLINE    – at least one connection, not queued or in a match
IN_QUEUE  – waiting in matchmaking
IN_MATCH  – in an active match
AWAY      – idle / explicitly away
```

Typical flow:

```
connect WS      OFFLINE  -> ONLINE     (implicit, on first connection)
join queue      ONLINE   -> IN_QUEUE
match found     IN_QUEUE -> IN_MATCH
match ends      IN_MATCH -> ONLINE
disconnect      *        -> OFFLINE    (only when the LAST connection closes)
```

### Transition rule (deliberately permissive)

The validator rejects **only** the two transitions that require a connection an
offline player does not have:

| Rejected            |
|---------------------|
| `OFFLINE -> IN_QUEUE` |
| `OFFLINE -> IN_MATCH` |

**Every other transition is allowed** — `ONLINE->IN_QUEUE`, `IN_QUEUE->IN_MATCH`,
`IN_MATCH->ONLINE`, `IN_MATCH->AWAY`, `AWAY->IN_QUEUE`, `IN_QUEUE->ONLINE`, etc.
Rejected transitions return `400` and increment
`gamemesh_presence_invalid_transitions_total`.

Why permissive? Future reconnect, rematch and party flows should never require
editing the state machine. The rule lives in `CanTransition`
([`model.go`](../internal/presence/model.go)).

`OFFLINE -> ONLINE` is not done via the API; it happens implicitly on the first
`connect`.

---

## Redis data model (source of truth — no SQL)

One key per player:

```
presence:{playerID}  ->  {
  "state":            "ONLINE",
  "last_seen":         1718900000,   // unix seconds
  "connection_count":  2,
  "updated_at":        1718900000
}
```

- **TTL** (default `45s`) on the key. Heartbeats refresh it; if they stop the key
  expires and the player is OFFLINE.
- A **missing key is OFFLINE** — there is no separate "offline" record to write.

### Why Redis and no SQL

Presence is ephemeral, high-churn and self-correcting. Durability buys nothing: a
lost record is re-derived from the next heartbeat. Redis gives O(1) reads, a
single pipelined bulk read for friend lists, and TTL as a free crash-recovery
mechanism. So presence intentionally does **not** use Postgres or the
transactional outbox (unlike player identity events).

### Atomic, replica-safe writes

Every mutating operation (`connect`, `disconnect`, `heartbeat`, `setState`) is a
**Lua script** ([`repository.go`](../internal/presence/repository.go)) that does
read → modify → write+EXPIRE atomically on the Redis server. Two WS replicas
connecting/disconnecting the same player concurrently can never lose an update or
clobber each other. (Verified by `TestConcurrentConnectsCountCorrectly`.)

---

## Heartbeat strategy

- WS replicas heartbeat every **15s** (`PRESENCE_HEARTBEAT_INTERVAL`), per
  locally-connected player, via the WS hub's heartbeat loop
  ([`hub.go`](../internal/wsgateway/hub.go) `RunHeartbeat`).
- Each heartbeat refreshes `last_seen`/`updated_at` and resets the **45s** TTL
  (`PRESENCE_TTL`) — a 3× margin, so two missed beats still leave the player
  online.
- If the key had already expired, a heartbeat **re-creates** it as ONLINE, so
  presence self-heals after a crash without an explicit reconnect.

---

## Multi-device support

A player may be on phone, tablet and browser at once. `connection_count` tracks
live connections:

- each `connect` increments it (ONLINE on the first);
- each `disconnect` decrements it;
- the player flips to **OFFLINE only when the count reaches 0** — at which point
  the key is deleted.

So closing one of three devices keeps the player ONLINE.

> **Future enhancement (not implemented):** replace the scalar `connection_count`
> with a set of connection IDs:
> ```
> connections:{ws-id-1, ws-id-2, ws-id-3}
> ```
> That enables reconnect, per-device metadata, browser/mobile distinction,
> connection ownership and session recovery. The current model is forward
> compatible — events already carry `connection_count`.

---

## Connection ownership & horizontal scaling

- All state is in Redis keyed by `playerID`; **any** WS replica can update **any**
  player. The atomic Lua writes make concurrent updates safe.
- **No sticky sessions, no leader election, no per-replica ownership.** Scale the
  Presence Service and the WS gateway horizontally by raising replicas
  ([`deployments/k8s/16-presence.yaml`](../deployments/k8s/16-presence.yaml),
  `replicas: 2`).

---

## Events

Published to NATS topic `events.presence` (subjects `events.presence.<Type>`):

| Type                   | When                                        |
|------------------------|---------------------------------------------|
| `PresenceOnline`       | OFFLINE → any non-offline state             |
| `PresenceOffline`      | any state → OFFLINE (last disconnect)       |
| `PresenceStateChanged` | any other state change (e.g. ONLINE→IN_QUEUE) |

Payload — [`events.PresenceChangedPayload`](../pkg/events/events.go):

```json
{ "player_id", "state", "previous_state", "connection_count", "last_seen" }
```

- Published via the shared `events.Publisher`, so trace context is propagated
  automatically and the transport (NATS/Redis) is config-selected.
- The `PRESENCE` JetStream stream has a short **15m** `MaxAge` — presence events
  are disposable; JetStream is not meant to replay long presence history (Redis
  is the source of truth).
- **Consumers are optional.** No service is required to subscribe; the flow is
  `WS → Presence → NATS → future consumers`, with no tight coupling.
- A publish failure is logged but never fails the operation — presence is
  re-derived from the next heartbeat.

---

## API (internal only)

These endpoints trust the cluster network and the supplied player IDs, like the
rest of GameMesh's internal services. No auth redesign.

| Method | Path                   | Purpose                                  |
|--------|------------------------|------------------------------------------|
| `GET`  | `/presence/:id`        | One player's presence (OFFLINE if absent). Read-only. |
| `POST` | `/presence/friends`    | Bulk lookup; body `{"ids":[...]}`. POST so large lists aren't URL-limited. |
| `PUT`  | `/presence/state`      | Explicit transition; body `{"player_id","state"}`. |
| `POST` | `/presence/connect`    | Register a connection (WS notifier).     |
| `POST` | `/presence/disconnect` | Drop a connection (WS notifier).         |
| `POST` | `/presence/heartbeat`  | Refresh TTL (WS notifier).               |

`IN_QUEUE` / `IN_MATCH` are driven through `PUT /presence/state` — matchmaking and
the WS gateway can call it; matchmaking itself is unchanged in this milestone.

### Friend lookup complexity

`GET many` issues N `GET`s in a **single pipeline** — **O(N) commands, one network
round trip, no N+1**. Missing keys map to OFFLINE.

---

## Failure & recovery scenarios

Redis TTL is the **sole** expiration mechanism. When heartbeats stop, the key
expires and the player is OFFLINE.

| Scenario          | Behaviour                                                        |
|-------------------|-----------------------------------------------------------------|
| **WS crash**      | Heartbeats stop → key TTL lapses → player OFFLINE. No explicit disconnect needed. |
| **Pod restart**   | Any replica resumes heartbeats; if within TTL the key survives, else the next heartbeat re-creates it ONLINE. |
| **Network partition** | Key expires during the partition; presence self-heals on reconnect. |
| **Presence Service down** | WS notifier calls fail softly (logged, async); presence catches up from later heartbeats. |

There is **no** reconciler, cron, SCAN worker, keyspace-notification subscriber or
lazy offline emission in this milestone. **Reads are side-effect free** — `GET`
and the friends lookup never publish events.

> **Future work:** emit explicit `PresenceOffline` on TTL expiry via Redis
> keyspace notifications or a lightweight reconciler (with a
> `presence_expired_total` metric). Not built now to keep the milestone narrowly
> scoped and avoid extra infrastructure.

---

## Observability

Tracing spans (tracer `github.com/alpnuhoglu/gamemesh`):
`presence.connect`, `presence.heartbeat`, `presence.transition`. (No
`presence.expire` — TTL expiry is passive Redis behaviour with no code path.)

Prometheus metrics ([`pkg/metrics`](../pkg/metrics/metrics.go), `gamemesh_` prefix,
`service` label):

| Metric                                       | Type        |
|----------------------------------------------|-------------|
| `gamemesh_presence_online_players`           | Gauge       |
| `gamemesh_presence_state_transitions_total`  | Counter (`from`,`to`) |
| `gamemesh_presence_heartbeat_total`          | Counter     |
| `gamemesh_presence_invalid_transitions_total`| Counter     |

---

## Foundation for the social layer

The `playerID`-keyed model and `PresenceChanged` events (carrying
`previous_state` and `connection_count`) give the next milestones what they need
without schema changes:

- **Friend Service** — `POST /presence/friends` for friend-list presence.
- **Party Service** — subscribe to `events.presence.*` for party member status.
- **Invites** — gate invites on a target's presence.
- **Reconnect** — `connection_count` (and the future connection-ID set) underpins
  session recovery.
- **Notifications / Push** — react to `PresenceOnline`/`PresenceOffline`.
- **Spectators** — presence of match participants.

The `events.presence.>` subject wildcard lets future consumers filter by type
without new streams.

---

## Configuration

| Env var                       | Default | Meaning                          |
|-------------------------------|---------|----------------------------------|
| `HTTP_PORT`                   | `8086`  | Presence Service port            |
| `PRESENCE_TTL`                | `45s`   | Presence key TTL                 |
| `PRESENCE_HEARTBEAT_INTERVAL` | `15s`   | WS heartbeat cadence             |
| `PRESENCE_SERVICE_URL`        | `http://localhost:8086` | WS gateway → Presence base URL |

---

## Testing

- Unit ([`internal/presence`](../internal/presence)): transitions, multi-device,
  heartbeat TTL refresh, expiry-reports-OFFLINE-without-events, reconnect,
  bulk friends, concurrency, handler routes. Backed by miniredis (Lua + TTL via
  `FastForward`).
- Integration ([`tests/integration/presence_test.go`](../tests/integration/presence_test.go),
  `-tags integration`): real Redis container exercising true TTL expiry,
  heartbeat keep-alive, multi-device and bulk friends.

Run: `go test ./internal/presence/...` and
`go test -tags integration ./tests/integration/...`.
