# GameMesh — Architectural Design Document

> **Author:** Principal Systems Architect
> **Status:** Baseline (v1) — reflects the as-built codebase
> **Audience:** Engineering, SRE, and technical reviewers

This document explains how each part of the GameMesh backend works in the
actual codebase, why each technical choice was made over its alternatives, and
how the system behaves under load and failure. It closes with the trade-offs we
knowingly accepted and how the design must evolve at 100× scale.

---

## 1. Executive Summary

GameMesh is a **scalable, real-time multiplayer game backend**. It handles the
lifecycle a game client depends on: account registration and authentication,
rank-based matchmaking, global leaderboards, and live event delivery (match
found, leaderboard changes, room join/leave).

### Architectural pattern

The system is a set of **stateless microservices behind an API gateway, glued
together by an event-driven backbone over Redis Pub/Sub.** Concretely:

- **Microservices** — five independently deployable processes
  ([cmd/](../cmd/)): `gateway`, `player`, `matchmaking`, `leaderboard`,
  `websocket`. Each owns a single bounded responsibility and a single port
  (8080–8084).
- **Event-driven core** — services do not call each other for real-time
  notifications. The matchmaking and leaderboard services *publish* domain
  events (`MatchFound`, `LeaderboardUpdated`); the WebSocket gateway
  *subscribes* and fans them out to clients. Publishers and subscribers never
  know about each other ([pkg/events/](../pkg/events/)).

### Why this pattern fits a game backend

Game traffic is **spiky, read-heavy on leaderboards, latency-sensitive on
matchmaking, and bursty on connections** (a tournament start, a regional
prime-time). Three properties of this pattern address that directly:

1. **Independent scaling.** Leaderboard reads, matchmaking ticks, and WebSocket
   fan-out have wildly different resource profiles. Splitting them into separate
   processes lets each scale on its own signal (RPS, queue depth, connection
   count) instead of over-provisioning a monolith for its hottest path.
2. **Decoupled real-time delivery.** A game must push events to clients the
   instant they happen. An event bus lets any number of WebSocket replicas
   receive every event without the producer tracking who is connected where —
   the producer fires once, the bus delivers everywhere.
3. **Blast-radius containment.** A bug or overload in matchmaking cannot take
   down logins or leaderboard reads; they are separate processes with separate
   failure domains.

The cost of this pattern (operational complexity, network hops, eventual
consistency on the event path) is deliberately bounded — see
[§4](#4-trade-offs-and-future-considerations).

---

## 2. Component Deep-Dive & Justification

### 2.1 Edge Layer

#### API Gateway (`:8080`) — [internal/gateway/](../internal/gateway/)

**Functionality.**
The gateway is the *only* public HTTP entry point. It is a thin reverse proxy
([proxy.go](../internal/gateway/proxy.go)) with a hardened middleware chain
([pkg/middleware/middleware.go](../pkg/middleware/middleware.go)):

- **JWT termination.** `middleware.Auth` validates the `Bearer` token once at
  the edge, extracts the player identity, and the proxy forwards it downstream
  as `X-User-ID` / `X-Token-JTI` headers. Crucially, the proxy *strips* any
  client-supplied identity headers before injecting the real ones
  ([proxy.go:51-58](../internal/gateway/proxy.go#L51-L58)) — clients can never
  spoof another user.
- **Rate limiting.** Per-IP token buckets (`golang.org/x/time/rate`), with a
  background janitor evicting idle IPs to bound memory
  ([middleware.go:156-184](../pkg/middleware/middleware.go#L156-L184)).
- **Request logging & request IDs.** Every request gets a `X-Request-ID`
  (generated if absent) propagated downstream, plus one structured zap log line
  with latency, status, and user ID — enabling cross-service tracing.
- **Explicit routing.** Routes are declared one-by-one, not via wildcards
  ([router.go](../internal/gateway/router.go)), so the gateway file *is* the
  reviewable, authoritative map of the public API surface. Public endpoints
  (register, login, leaderboard reads) skip auth; everything else sits behind
  the `protected` group.
- **WebSocket upgrade proxying.** `httputil.ReverseProxy` transparently proxies
  WS upgrades, so `/ws` can route through the same gateway.

**Architectural reasoning.**
We centralize cross-cutting concerns (authn, rate limiting, logging) at the edge
so the internal services stay simple and consistent. The alternative —
duplicating JWT validation in every service — means five places to get auth
wrong and five places to update when it changes. The trade-off is an explicit
**trust boundary**: internal services trust `X-User-ID` because they are only
reachable on the cluster network ([architecture.md](architecture.md#L27-L30)),
not from the internet.

**Scalability & resilience.**
- The gateway is **stateless** → horizontally scalable behind a load balancer
  (HPA on CPU/RPS).
- **Graceful upstream degradation:** the proxy's `ErrorHandler` converts a dead
  upstream into a clean `502 {"error":"upstream unavailable"}` and logs it
  rather than hanging or leaking a stack trace
  ([proxy.go:38-46](../internal/gateway/proxy.go#L38-L46)).
- **Known limitation:** rate-limit buckets are per-instance, so the effective
  global limit scales with replica count. The fix (Redis-backed sliding window)
  is called out at the interface boundary and noted in §4.

#### WebSocket Gateway (`:8084`) — [internal/wsgateway/](../internal/wsgateway/)

**Functionality.**
Maintains live client connections and pushes server events to them.

- **Connection lifecycle.** On `/ws` upgrade it validates the JWT *itself*
  ([handler.go:62-91](../internal/wsgateway/handler.go#L62-L91)). Browsers can't
  set an `Authorization` header on a WS handshake, so the token arrives as
  `?token=` (Bearer header also accepted for non-browser clients). This is the
  one deliberate exception to "auth lives only at the gateway."
- **Hub & rooms.** An in-memory `Hub` tracks `clients`, `byPlayer` (multi-tab
  support), and `rooms`, guarded by a single `RWMutex` — broadcasts take the
  read lock, membership changes the write lock
  ([hub.go](../internal/wsgateway/hub.go)).
- **Event bridge.** Each replica runs a `Bridge` subscribed to the matchmaking
  and leaderboard topics; it dispatches `MatchFound` to the specific matched
  players and `LeaderboardUpdated` to everyone
  ([bridge.go](../internal/wsgateway/bridge.go)).
- **Connection hygiene.** Standard gorilla pattern: one reader + one writer
  goroutine per connection, ping/pong keepalive, read deadlines, and a max
  message size ([client.go](../internal/wsgateway/client.go)).

**Architectural reasoning.**
We separated the WebSocket gateway from the API gateway because the two have
**opposite lifecycles**: HTTP is short, request/response, CPU-bound; WebSockets
are long-lived, memory-bound (one goroutine pair + buffer per connection), and
scale on connection *count*, not RPS. Co-locating them would force one scaling
policy onto two incompatible workloads.

**Scalability & resilience.**
- **No sticky sessions required.** Every replica subscribes to every topic via
  Pub/Sub, so a client can land on *any* replica behind a plain round-robin
  Service and still receive all its events
  ([bridge.go:13-19](../internal/wsgateway/bridge.go#L13-L19)). This is the
  single most important scaling property of the WS tier.
- **Backpressure by load-shedding.** `trySend` does a non-blocking channel send
  and *drops* the message if a client's 64-deep buffer is full
  ([client.go:44-50](../internal/wsgateway/client.go#L44-L50)). A slow consumer
  can never block the broadcast path for everyone else. This is an explicit
  availability-over-completeness choice, acceptable because authoritative state
  is re-queryable (the dropped event's truth still lives in Redis).

### 2.2 Services Layer

#### Player Service (`:8081`) — [internal/player/](../internal/player/)

**Functionality.**
Registration, login, logout, and profile management, structured as
**handler → service → repository** ([service.go](../internal/player/service.go)).

- **Register:** bcrypt-hash password (cost 12), create a `players` row and a
  default `player_stats` row (rank 1000) in one GORM create.
- **Login:** look up by username or email, verify password, issue an HS256 JWT
  (24h, carrying player ID + username + a JTI), and cache the session in Redis
  keyed by JTI ([session.go](../internal/player/session.go)).
- **Logout:** delete the session key → server-side revocation of an otherwise
  stateless JWT.

**Architectural reasoning.**
- **JWT (stateless) + a session cache (stateful escape hatch).** Pure stateless
  JWTs can't be revoked before expiry; pure server-side sessions cost a DB/Redis
  hit on every request. We take the best of both: requests validate
  cryptographically (no lookup), but a JTI-keyed session key with
  `SET ... EX = token expiry` ([session.go:33-35](../internal/player/session.go#L33-L35))
  gives us revocation and automatic TTL cleanup — no cron, no table bloat.
- **Uniform `ErrInvalidCredentials`.** "No such user" and "wrong password"
  return the *same* error to prevent account enumeration
  ([service.go:14-16](../internal/player/service.go#L14-L16)).
- **Best-effort session write.** A Redis blip on login logs a warning but does
  *not* fail the login ([service.go:81-84](../internal/player/service.go#L81-L84))
  — graceful degradation on the auth hot path.

**Scalability & resilience.** Stateless → HPA on CPU/RPS. Identity data shards
naturally by player ID. The `players` / `player_stats` split (below) keeps
hot gameplay writes off the identity row.

#### Matchmaking Service (`:8082`) — [internal/matchmaking/](../internal/matchmaking/)

**Functionality.**
Players `POST /queue {rank}`; a background loop pairs them every 5s
([service.go](../internal/matchmaking/service.go)).

Each tick (`MatchOnce`):
1. **Evict stale tickets** older than `MATCH_MAX_QUEUE_AGE` — covers
   disconnected clients that never sent an explicit leave
   ([queue.go:120-137](../internal/matchmaking/queue.go#L120-L137)).
2. **Snapshot** up to `MATCH_BATCH_SIZE` tickets via `ZRANGE` — already
   **rank-sorted** by the ZSET, so no sort step
   ([queue.go:104-115](../internal/matchmaking/queue.go#L104-L115)).
3. **Pair** with a pure, unit-tested function: greedy adjacent pairing under a
   max rank gap, O(n) ([matcher.go](../internal/matchmaking/matcher.go)).
4. Per pair: create a room UUID, persist room JSON with TTL, `ZREM` both
   players, and **publish `MatchFound`**.

**Architectural reasoning.**
- **Redis ZSET (score = rank) over a SQL queue table.** This is the central
  data-structure decision. In a SQL approach, every tick would run
  `SELECT ... ORDER BY rank LIMIT n` — an O(n log n) sort (or constant index
  maintenance on a write-heavy, churning table) plus the row-locking dance of
  deleting matched players. With a ZSET the queue is *permanently* sorted by
  rank, so the tick reads players already ordered for windowed pairing in
  O(log n + m), `ZCARD` gives O(1) queue depth for metrics, and `ZREM` is
  O(log n). The pairing itself collapses to a single O(n) walk of adjacent
  entries — no sort, no locks, no index bloat.
- **Pure pairing function.** Keeping `Pair` free of Redis makes the *algorithm*
  (the part most likely to change: dynamic windows, MMR, party constraints)
  trivially unit-testable in isolation.
- **A separate join-time hash.** The ZSET score encodes *rank*, not time, so we
  can't use it to find stale tickets. A small `matchmaking:queue:joined` hash
  holds join timestamps for O(1) eviction scans without disturbing rank order.

**Scalability & resilience.**
- The match loop is **a singleton by design.** Running it on N replicas would
  double-match players. The K8s deployment runs **one** matchmaking replica,
  which comfortably handles thousands of matches per tick
  ([service.go:61-80](../internal/matchmaking/service.go#L61-L80)). The
  documented path to scale-out is a Redis leader lock (`SET NX EX`) or
  partitioning the queue into rank bands with one worker per band.
- **Self-healing queue.** Stale eviction means a crashed client doesn't poison
  the queue forever.

#### Leaderboard Service (`:8083`) — [internal/leaderboard/](../internal/leaderboard/)

**Functionality.**
Global rankings on a single Redis sorted set `leaderboard:global`
([store.go](../internal/leaderboard/store.go)): `SetScore` (`ZADD`),
`IncrScore` (`ZINCRBY`), `Top`/`Page` (`ZREVRANGE`), `Rank` (`ZREVRANK`). A
score update publishes `LeaderboardUpdated`.

**Architectural reasoning — ZSET vs. SQL, explicitly.**
A leaderboard is the textbook case for a sorted set. The required operations
and their complexities:

| Operation | ZSET | Naive SQL |
|---|---|---|
| Update one score | `ZADD`/`ZINCRBY` — **O(log n)** | `UPDATE` + index maintenance |
| "What's my rank?" | `ZREVRANK` — **O(log n)** | `COUNT(*) WHERE score > x` — **O(n)** |
| Top-N / page | `ZREVRANGE` — **O(log n + m)** | `ORDER BY score DESC LIMIT` — sort over the set |

The killer is **"what is player X's rank?"** — in SQL that's a full count of
everyone above them (O(n)) on every read; in a ZSET it's O(log n) and stays fast
at millions of members. We accept that the leaderboard is *in-memory and not the
system of record* (durable scores live in `player_stats`); the ZSET is a
purpose-built read model that can always be rebuilt from Postgres.

**Scalability & resilience.** Stateless service → HPA. Multiple boards
(per-season, per-mode) are simply more keys, e.g. `leaderboard:{season}`
([store.go:17-19](../internal/leaderboard/store.go#L17-L19)) — a natural sharding
seam.

### 2.3 Data Layer

#### PostgreSQL — relational source of truth

**Functionality.** Holds `players` (identity) and `player_stats` (rank, score,
games_played) in a 1:1 relationship.

**Architectural reasoning.**
- **Relational store for identity** because the data is relational, needs
  uniqueness constraints (`username`, `email`), transactional integrity on
  registration, and is the durable system of record. Redis is a cache/index in
  this system, never the place identity lives.
- **`player_stats` split from `players`.** Hot gameplay writes (rank/score
  churning constantly) never contend with — or bloat the row version of — the
  rarely-changing identity row.

**Scalability & resilience.** Single node today; the path is managed Postgres
with read replicas, sharding by player ID (which the UUID PK supports naturally).

#### Redis — in-memory engine for everything latency-critical

**Functionality.** Redis is doing four distinct jobs, each with a data structure
chosen for it:

| Use case | Structure | Key | Why |
|---|---|---|---|
| Leaderboard | Sorted set | `leaderboard:global` | O(log n) rank/update, O(log n + m) ranges |
| Matchmaking queue | Sorted set (score = rank) | `matchmaking:queue` | Permanently rank-sorted → O(n) pairing, no sort |
| Queue join times | Hash | `matchmaking:queue:joined` | O(1) timestamp ops for stale eviction |
| Room cache | String + TTL (JSON) | `room:{uuid}` | Read/written whole; TTL auto-purges abandoned rooms |
| Session cache | String + TTL | `session:{jti}` | TTL = token expiry; enables revocation |
| Events | Pub/Sub | `events.*` | Sub-ms fan-out to all WS replicas |

**Architectural reasoning — why Pub/Sub here.**
The event path is **fire-and-forget real-time notification**, and Pub/Sub is the
right tool *because* of, not despite, its at-most-once delivery. A `MatchFound`
or `LeaderboardUpdated` message that's missed is self-repairing: the
authoritative state (the room key, the leaderboard ZSET) is still queryable, and
the next leaderboard poll or match tick reconciles it
([redis.go:11-17](../pkg/events/redis.go#L11-L17)). We don't pay for the
durability, ordering, and consumer-group machinery of Kafka/NATS JetStream for a
signal whose ground truth lives elsewhere. The flip side — when we *do* need
at-least-once delivery with replay — is a one-implementation swap, because every
service depends only on the `events.Publisher` / `events.Subscriber` interfaces
([pkg/events/events.go](../pkg/events/events.go)), never on Redis directly. A
`NATSBus` or `KafkaBus` satisfies the same two methods; only `cmd/*/main.go`
changes.

**Scalability & resilience.** Single node today; path is Redis Cluster, with the
leaderboard sharding by season/mode keys and Pub/Sub graduating to NATS/Kafka
when delivery guarantees matter.

### 2.4 Observability — [pkg/metrics/](../pkg/metrics/), [pkg/logger/](../pkg/logger/)

**Functionality.**
- Every service exposes `/metrics` (Prometheus) and `/healthz`.
- **Metrics:** per-route request count / error count / latency histograms (using
  the matched *route template*, not the raw URL, to bound label cardinality —
  [middleware.go:81-97](../pkg/middleware/middleware.go#L81-L97)), plus
  domain gauges: active WS connections, matchmaking queue depth, matches
  created, leaderboard updates.
- **Structured logs:** one zap line per request with `service`, `request_id`,
  `user_id`, latency, status, and error — the `request_id` threads a single
  request across service boundaries.
- **Grafana** auto-provisioned with a "GameMesh Overview" dashboard.

**Architectural reasoning.**
- **Per-service Prometheus registry** (not the global default) so tests don't
  collide on a shared registry and only intentional metrics are exposed.
- **Route-template labels** are a deliberate cardinality-control decision — using
  raw paths (`/players/<uuid>`) would explode the time-series count and melt
  Prometheus.

**Scalability & resilience.** This *is* the resilience tooling: queue-depth and
connection-count gauges drive the HPA signals and alerting that the scaling
strategy in §4 depends on. Pull-based scraping means a scrape failure degrades
visibility, not the service.

---

## 3. Core Data Flows

### Flow A — Authenticate → Connect → Enter Queue

```
Client            Gateway:8080      Player:8081     Postgres   Redis      WS:8084
  │  POST /login        │                │              │         │          │
  ├────────────────────►│  proxy /auth/login            │         │          │
  │                     ├───────────────►│ lookup user  │         │          │
  │                     │                ├─────────────►│         │          │
  │                     │                │ verify bcrypt│         │          │
  │                     │                │ issue JWT(jti)│        │          │
  │                     │                │ SET session:{jti} EX   │          │
  │                     │                ├────────────────────────►         │
  │  200 {token}        │◄───────────────┤              │         │          │
  │◄────────────────────┤                │              │         │          │
  │                                                                          │
  │  WSS /ws?token=JWT  ───────────────────────────────────────────────────►│
  │                          (WS validates JWT itself, no gateway auth)      │ register in hub
  │◄─────────────────────────────────────────────────────────── connected ──┤
  │                                                                          │
  │  POST /queue {rank}  │                                                   │
  ├─────────────────────►│ Auth(JWT)→X-User-ID → proxy → Matchmaking:8082    │
  │                      │   ZADD matchmaking:queue (score=rank)             │
  │                      │   HSET matchmaking:queue:joined                   │
  │  202 queued          │◄──────────────                                    │
  │◄─────────────────────┤                                                   │
```

Step by step:
1. **Login.** `POST /api/v1/auth/login` → gateway proxies to the player service.
   The service verifies bcrypt, issues an HS256 JWT carrying player ID +
   username + JTI, and writes `session:{jti}` to Redis with TTL = token expiry
   ([service.go:62-87](../internal/player/service.go#L62-L87)). Token returns to
   the client.
2. **Connect.** Client opens `wss://…/ws?token=<JWT>`. The WS gateway validates
   the token *itself* (query param), upgrades the connection, and registers the
   client in its in-memory hub
   ([handler.go:62-91](../internal/wsgateway/handler.go#L62-L91)). The bridge is
   already subscribed to the event topics.
3. **Enter queue.** Client `POST /api/v1/queue {rank}` with the Bearer token.
   The gateway's `Auth` middleware validates the JWT, injects `X-User-ID`, and
   proxies to matchmaking, which `ZADD`s the player into `matchmaking:queue`
   (score = rank) and records the join time
   ([queue.go:48-54](../internal/matchmaking/queue.go#L48-L54)). The player is
   now queued and listening on WebSocket.

### Flow B — Match found → Players transition into a room

```
Matchmaking:8082         Redis                     WS:8084 (any replica)     Clients
  │  (every 5s tick)        │                            │                      │
  │  ZRANGE queue (sorted)  │                            │                      │
  ├────────────────────────►│                            │                      │
  │  Pair() greedy O(n)     │                            │                      │
  │  per pair:              │                            │                      │
  │   SET room:{id} EX      │                            │                      │
  ├────────────────────────►│                            │                      │
  │   ZREM both players     │                            │                      │
  ├────────────────────────►│                            │                      │
  │   PUBLISH events.matchmaking {MatchFound}            │                      │
  ├────────────────────────►│ ── Pub/Sub ──────────────►│ Bridge.dispatch      │
  │                         │                            │ SendToPlayer(p1,p2)  │
  │                         │                            ├─────────────────────►│ {type:MatchFound, room}
  │                         │                            │                      │
  │                         │       client: {action:join, room} ──────────────►│ readPump
  │                         │                            │ hub.join → rooms[id] │
  │                         │                            │ BroadcastToRoom      │
  │                         │                            ├─────────────────────►│ {type:PlayerJoined}
```

Step by step:
1. **Tick.** Every 5s the singleton match loop snapshots the rank-sorted queue,
   evicts stale tickets, and runs the pure `Pair` function
   ([service.go:85-138](../internal/matchmaking/service.go#L85-L138)).
2. **Room creation.** For each pair: a room UUID + JSON is written to Redis with
   a TTL, both players are `ZREM`'d from the queue, the `MatchesCreated` metric
   ticks, and a `MatchFound` event (room ID + player IDs) is published to
   `events.matchmaking`
   ([service.go:98-132](../internal/matchmaking/service.go#L98-L132)).
3. **Fan-out.** Redis Pub/Sub delivers the event to **every** WS replica. Each
   replica's bridge unmarshals it and — because matched players may not be in
   any room yet — targets them directly with `SendToPlayer`
   ([bridge.go:46-58](../internal/wsgateway/bridge.go#L46-L58)). The two matched
   players receive `{type: "MatchFound", room: "<id>"}` regardless of which
   replica they're connected to.
4. **Join the room.** Each client responds with `{action: "join", room: "<id>"}`
   over its socket. The reader registers them in the hub's room map and
   broadcasts `PlayerJoined` to the room's occupants
   ([client.go:80-85](../internal/wsgateway/client.go#L80-L85),
   [hub.go:88-98](../internal/wsgateway/hub.go#L88-L98)). Both players are now in
   a shared real-time room; subsequent in-game events broadcast room-scoped.

The key architectural point in Flow B: **the producer (matchmaking) never knows
where the players are connected.** It publishes once; the bus and the
per-replica bridges handle delivery. That's what makes the WS tier horizontally
scalable without sticky sessions.

---

## 4. Trade-offs and Future Considerations

No architecture is free. We made these trade-offs deliberately and documented
the seams to undo them.

### Trade-off 1 — At-most-once event delivery (Pub/Sub)

**What we accepted.** Redis Pub/Sub has no persistence, no replay, no consumer
acks. If a WS replica is mid-restart when a `MatchFound` fires, that message is
gone for clients on that replica.

**Why it's acceptable today.** Every event's ground truth is re-queryable
(`room:{id}` exists, the leaderboard ZSET is current), and clients reconcile on
their next poll/tick. We traded delivery guarantees for sub-ms fan-out and
near-zero operational weight.

**When it breaks & the fix.** The moment an event carries information that *isn't*
recoverable from state — currency grants, "you won" notifications, anything
financial — at-most-once is unsafe. The migration is already scoped: implement
`events.Publisher`/`events.Subscriber` over **NATS JetStream or Kafka**
([events.go](../pkg/events/events.go), [redis.go](../pkg/events/redis.go)),
upgrading to at-least-once with replay and consumer groups. Zero service code
changes — only the constructor in `cmd/*/main.go`.

### Trade-off 2 — Single matchmaking replica

**What we accepted.** The match loop is a singleton; it cannot scale
horizontally as written, and it's a single point of failure for *new* matches
(existing rooms and queueing are unaffected — those are just Redis ops).

**Why it's acceptable today.** A single tick pairs thousands of players in an
O(n) walk; one replica saturates long before the algorithm does. Sharing one
correct matcher is far simpler than coordinating N matchers fighting over the
same queue ([service.go:61-64](../internal/matchmaking/service.go#L61-L64)).

**When it breaks & the fix.** When tick latency (queue size / pairing time)
exceeds the 5s budget, or when single-replica downtime on new matches becomes
unacceptable. Two documented paths: a **Redis leader lock** (`SET NX EX`) so a
warm standby takes over on failure, or **partition the queue into rank bands**
(`matchmaking:queue:{band}`) with one worker per band — which also restores
horizontal scaling.

### Other accepted trade-offs (briefly)

- **Per-instance rate limiting** — global limit scales with replica count; fix
  is a Redis sliding-window limiter behind the same `gin.HandlerFunc` seam
  ([middleware.go:128-131](../pkg/middleware/middleware.go#L128-L131)).
- **Internal trust boundary** — services trust `X-User-ID` on the assumption the
  cluster network is private; zero-trust upgrade is mTLS or signed short-lived
  internal JWTs.
- **In-memory WS hub** — room membership is per-replica; it works because event
  delivery goes through Pub/Sub, but room *state* doesn't survive a replica
  restart (clients re-join). Fine for ephemeral game rooms; not for durable
  channels.

### Evolving for 100× player base

| Component | Today | At 100× |
|---|---|---|
| Gateway / Player / Leaderboard / WS | 2 replicas | Stateless → HPA on CPU/RPS. Shard WS event topics (`events.room.{hash}`) when broadcast fan-out volume dominates. |
| Matchmaking | 1 replica | Rank-band partitioning, one worker per band, behind leader locks. |
| PostgreSQL | single node | Managed Postgres + read replicas; shard by player-ID UUID. |
| Redis | single node | Redis Cluster; leaderboard sharded by season/mode; Pub/Sub → NATS/Kafka for guaranteed delivery. |
| Rate limiting | per-instance buckets | Redis-backed sliding window, consistent across gateway replicas. |
| Events | Redis Pub/Sub (at-most-once) | Kafka/NATS JetStream (at-least-once + replay) on the same interfaces. |

The unifying theme: **the expensive parts of this evolution are already isolated
behind interfaces** (`events.Publisher`/`Subscriber`, the repository/service
seams, the `gin.HandlerFunc` middleware boundary). Scaling GameMesh by 100× is a
sequence of swapping implementations behind stable seams, not rewriting the
services — which is the property we optimized the v1 design to preserve.
