# API Reference

Base URL: `http://localhost:8080` (gateway). All bodies are JSON.
Protected endpoints require `Authorization: Bearer <token>`.
Errors use a uniform envelope: `{"error": "<message>"}`.

## Auth

### POST /api/v1/auth/register
```json
{ "username": "neo", "email": "neo@example.com", "password": "password123" }
```
Validation: username alphanumeric 3–32; valid email; password 8–72 chars.
**201** → player object (no password hash). **409** if username/email taken.

### POST /api/v1/auth/login
```json
{ "identifier": "neo", "password": "password123" }
```
`identifier` = username **or** email.
**200** → `{ "token": "<jwt>", "player": { … } }`. **401** on bad credentials
(identical response whether the user exists or not).

### POST /api/v1/auth/logout 🔒
Revokes the current session (deletes the Redis session for the token's JTI).
**200** → `{ "status": "logged out" }`

## Players

### GET /api/v1/players/:id 🔒
`:id` accepts a UUID or the literal `me`.
**200** → `{ "id", "username", "email", "created_at", "updated_at", "stats": { "rank", "score", "games_played" } }`
**404** if not found.

### PUT /api/v1/players/:id 🔒
```json
{ "username": "trinity", "email": "new@example.com" }
```
Both fields optional. Players may only update their own profile → **403**
otherwise. **409** on uniqueness conflict.

## Leaderboard

### POST /api/v1/score 🔒
```json
{ "score": 150, "increment": true }
```
`increment: true` adds to the current score; `false`/omitted overwrites.
Player identity comes from the JWT.
**200** → `{ "player_id", "score", "rank" }`

### GET /api/v1/leaderboard?offset=0&limit=50
**200** → `{ "entries": [{ "player_id", "score", "rank" }], "total", "offset", "limit" }`

### GET /api/v1/leaderboard/top/:n
`n` between 1 and 1000.
**200** → `{ "entries": [ … ] }`

### GET /api/v1/leaderboard/rank/:player_id
**200** → `{ "player_id", "score", "rank" }`. **404** if unranked.

## Matchmaking

### POST /api/v1/queue 🔒
```json
{ "rank": 1000 }
```
Idempotent — re-joining refreshes rank and join time.
**200** → `{ "status": "queued", "rank": 1000 }`

### DELETE /api/v1/queue 🔒
Leaves the queue. **404** if not queued.

### GET /api/v1/queue/status 🔒
**200** → `{ "queued": true, "queue_size": 42 }`

### GET /api/v1/rooms/:id 🔒
**200** → `{ "id", "players": ["…"], "ranks": {"…": 1000}, "status": "waiting", "created_at" }`
**404** if missing/expired.

## WebSocket

### GET /ws?token=&lt;jwt&gt;
Connect via gateway (`:8080/ws`) or directly (`:8084/ws`). The token may also
be sent as an `Authorization: Bearer` header (non-browser clients).

**Client → server**
```json
{ "action": "join",  "room": "<room-id>" }
{ "action": "leave", "room": "<room-id>" }
```

**Server → client**

| Type | Trigger | Data |
|---|---|---|
| `PlayerJoined` | someone joins a room you're in | `{ "player_id" }` |
| `PlayerLeft` | someone leaves / disconnects | `{ "player_id" }` |
| `MatchFound` | matchmaking paired you | `{ "room_id", "players": […] }` (sent directly to both players) |
| `LeaderboardUpdated` | any score change | `{ "player_id", "score", "rank" }` (broadcast) |

## Operational endpoints (every service)

- `GET /healthz` → `{ "status": "ok", "service": "<name>" }`
- `GET /metrics` → Prometheus exposition format
