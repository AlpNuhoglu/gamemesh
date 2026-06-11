# Architecture

## Service responsibilities

| Service | Port | Owns | Stores |
|---|---|---|---|
| API Gateway | 8080 | Public entry, JWT termination, rate limiting, routing, request logging | — |
| Player | 8081 | Registration, login, profiles | PostgreSQL (`players`, `player_stats`), Redis (sessions) |
| Matchmaking | 8082 | Queueing, rank-window pairing, room creation, stale eviction | Redis (queue ZSET, join-time hash, room JSON) |
| Leaderboard | 8083 | Global rankings, top-N, score updates | Redis (ZSET) |
| WebSocket | 8084 | Connections, room subscriptions, event broadcast | in-memory hub (per replica) |

## Request flows

### Authentication

1. `POST /api/v1/auth/register` → gateway proxies to player service → bcrypt
   (cost 12) hash → `players` + default `player_stats` rows in one GORM create.
2. `POST /api/v1/auth/login` → credential check → HS256 JWT issued (24h,
   carries player ID + username + JTI) → session cached in Redis as
   `session:{jti}` with TTL = token expiry.
3. Subsequent requests: the **gateway** validates the JWT and forwards
   identity via `X-User-ID` / `X-Token-JTI` headers. The gateway strips those
   headers from inbound external requests first — clients can never spoof
   them.

**Trust boundary.** Internal services trust `X-User-ID` because they are only
reachable on the cluster/compose network. This keeps auth logic in exactly one
place. The hardening step for zero-trust networks is mTLS or a signed
internal header (e.g. short-lived service JWT) — the seam is already there.

### Matchmaking

1. Client `POST /api/v1/queue {rank}` → ZADD into `matchmaking:queue`
   (score = rank) + join timestamp into a hash.
2. Every 5 s (configurable) the match loop:
   - evicts tickets older than `MATCH_MAX_QUEUE_AGE` (disconnected players),
   - takes a rank-ordered snapshot (`ZRANGE`, capped at `MATCH_BATCH_SIZE`),
   - runs the **pure** pairing function: greedy adjacent pairing under a max
     rank gap — O(n), optimal for pair count on a sorted list, unit-tested in
     isolation,
   - per pair: creates a room UUID, stores room JSON with TTL, `ZREM`s both
     players, publishes `MatchFound`.
3. The WS gateway receives `MatchFound` via Pub/Sub and pushes it to each
   matched player's connections.

### Real-time events

```
matchmaking ──MatchFound──────────► Redis Pub/Sub ──► every WS replica ──► targeted players
leaderboard ──LeaderboardUpdated──► Redis Pub/Sub ──► every WS replica ──► broadcast
client join/leave room ────────────────────────────► hub ──► PlayerJoined/PlayerLeft to room
```

Every WS replica subscribes to every topic, so clients can connect to any
replica behind a plain round-robin Service — no sticky sessions needed for
event delivery.

## Redis data structure choices

| Use case | Structure | Why |
|---|---|---|
| Leaderboard | **Sorted set** `leaderboard:global` | O(log n) `ZADD`/`ZINCRBY`/`ZREVRANK`; O(log n + m) ranged reads. Exactly the required bounds; proven at millions of members. |
| Matchmaking queue | **Sorted set** `matchmaking:queue` (score = rank) | Queue stays permanently rank-sorted → each tick pairs in O(n) without sorting. `ZCARD` gives O(1) queue depth for metrics. |
| Queue join times | **Hash** `matchmaking:queue:joined` | O(1) field ops; lets stale-eviction scan timestamps without touching the ZSET scores (which encode rank, not time). |
| Room cache | **String (JSON) + TTL** `room:{uuid}` | Rooms are read-whole/write-whole; TTL auto-purges abandoned rooms — no janitor process. |
| Session cache | **String + TTL** `session:{jti}` | Lifetime exactly matches JWT expiry; enables logout/revocation for stateless tokens. |
| Events | **Pub/Sub** `events.*` | Sub-ms fan-out to all WS replicas; at-most-once is acceptable because authoritative state (ZSET, room keys) is re-queryable. |

## Transport migration path (REST → gRPC/NATS/Kafka)

Two seams isolate transport from business logic:

1. **`events.Publisher` / `events.Subscriber`** (`pkg/events`) — services hold
   the interface, `RedisBus` is one implementation. A `NATSBus`/`KafkaBus`
   implements the same two methods; `cmd/*/main.go` swaps the constructor.
   Kafka would upgrade delivery to at-least-once with replay.
2. **Repository + service interfaces** — handlers depend on services, services
   on repositories. Adding a gRPC server is a new adapter calling the same
   service layer; REST and gRPC can run side by side during migration.

## Scaling to millions of players

| Component | Today (Minikube) | At scale |
|---|---|---|
| Gateway / Player / Leaderboard / WS | 2 replicas | Stateless → HPA on CPU/RPS. WS replicas all receive all events; shard topics (`events.room.{hash}`) when fan-out volume demands. |
| Matchmaking | 1 replica | Leader lock (Redis `SET NX EX`) or partition the queue into rank bands, one worker per band. |
| PostgreSQL | single node | Managed Postgres, read replicas; identity data shards naturally by player ID. |
| Redis | single node | Redis Cluster; leaderboard shards by season/mode keys; Pub/Sub → NATS/Kafka when delivery guarantees matter. |
| Rate limiting | per-instance token buckets | Redis-backed sliding window so limits hold across gateway replicas. |

## Sample logs

```json
{"level":"info","ts":"2026-06-11T14:02:11.482+0000","caller":"middleware/middleware.go:74","msg":"http request","service":"gateway","request_id":"7f3b0a52-9c1e-4e0f-b1a3-0d2f6f9b8e21","method":"POST","path":"/api/v1/auth/login","status":200,"latency":"248ms","client_ip":"172.20.0.1","user_id":"11111111-1111-1111-1111-111111111111"}
{"level":"info","ts":"2026-06-11T14:02:13.005+0000","caller":"matchmaking/service.go:142","msg":"match created","service":"matchmaking","room_id":"a3e8b9c2-4d51-4f7e-9a01-c2d3e4f5a6b7","players":["1111…","2222…"]}
{"level":"error","ts":"2026-06-11T14:02:14.110+0000","caller":"middleware/middleware.go:71","msg":"http request","service":"player","request_id":"3c9d8e7f-2b1a-4c5d-8e9f-0a1b2c3d4e5f","method":"POST","path":"/auth/login","status":401,"latency":"251ms","client_ip":"172.20.0.5","error":"invalid credentials"}
```
