# In-game latency

How long one player's message takes to reach the other player over WebSocket,
how that time splits between the server and everything around it, and what each
change to the server did to it. Updated after every change.

The 112 ms p95 quoted elsewhere is HTTP request latency under load. It says
nothing about this path.

## What is measured

A client sends `{"action":"relay","room":"…","data":…}`; the WebSocket service
fans it out from its in-memory hub to the room's other members as
`{"type":"Relay","room":"…","from":"<player>","data":…}`
(`internal/wsgateway/relay.go`). The message does not touch NATS JetStream,
Redis or the outbox.

`scripts/latencybench` creates 2N players, pairs them through matchmaking, and in
each pair sends 550 messages of 120 bytes, one every 50 ms, from one player to
the other. The first 50 are warm-up and discarded.

| Stage | Where the clock is | How |
|---|---|---|
| Client sent → client received | The bench process | Both players are in one process, so the send time (`time.Since(processStart)`, monotonic) travels in the payload and is subtracted on receipt. Percentiles are exact, over all pairs pooled. |
| Server read → fan-out queued (`process`) | The server | `gamemesh_ws_relay_duration_seconds{stage}`, scraped before and after the counted window. The mean is exact (Δsum/Δcount); percentiles are interpolated within histogram buckets. |
| Wait in the receiver's send buffer (`queue`) | The server | Same histogram. |
| Frame write (`write`) | The server | Same histogram. |
| Server read → server wrote (`total`) | The server | Same histogram. |
| Network | The bench process | WebSocket ping → pong round trip on every connection. A cross-check, not a subtraction: pings are sparse (20 per connection) and travel as control frames. |

Client and server clocks are never subtracted from each other. "Outside the
server" below is end-to-end minus the server's `total`.

## Baseline

Measured 2026-11-11 on commit `2b233e8`, before any latency change.

**Environment.** Bench and server on the same machine: Apple M4 Pro (14 cores),
macOS 27.0.1, Docker Desktop 29.9.2 (Linux VM, 14 CPUs, 8 GB). The server is
the `docker compose` stack, one websocket container; the bench runs natively on
macOS and reaches it at `ws://localhost:8084`, so every message crosses Docker
Desktop's port forwarding twice. There is no real network here: these numbers
describe the server and the local plumbing, not what a player sees. Production
numbers (`api.gamemesh.app`) will be added separately and are the ones to quote.

### End to end, direct to the websocket service

Three runs per row; the value is the median of the three, the range is their
minimum and maximum. No run lost, duplicated or reordered a message, and the
server shed and rejected none.

| Pairs | Samples per run | Lost | p50 ms | p95 ms | p99 ms | max ms |
|---|---|---|---|---|---|---|
| 1 | 500 | 0 | 1.295 (1.281–1.610) | 2.147 (2.013–2.295) | 2.824 (2.514–2.851) | 3.889 (3.559–5.871) |
| 10 | 5 000 | 0 | 0.542 (0.537–0.563) | 1.142 (1.072–1.293) | 1.918 (1.563–1.954) | 3.787 (3.638–7.611) |
| 50 | 25 000 | 0 | 0.283 (0.280–0.299) | 0.392 (0.389–0.422) | 0.599 (0.587–0.603) | 4.545 (2.734–6.514) |

### Inside the server

Median of the same three runs. Means are exact; p95 and p99 are bucket
estimates.

| Pairs | process mean | queue mean | write mean | total mean | total ~p95 | total ~p99 |
|---|---|---|---|---|---|---|
| 1 | 0.057 ms | 0.030 ms | 0.062 ms | 0.131 ms | 0.245 ms | 0.404 ms |
| 10 | 0.023 ms | 0.014 ms | 0.030 ms | 0.058 ms | 0.173 ms | 0.240 ms |
| 50 | 0.009 ms | 0.006 ms | 0.012 ms | 0.022 ms | 0.043 ms | 0.050 ms |

### Where the time goes

| Pairs | End-to-end p50 | Server total (mean) | Outside the server | Ping RTT p50 |
|---|---|---|---|---|
| 1 | 1.295 ms | 0.131 ms (10%) | 1.164 ms (90%) | 0.983 ms |
| 10 | 0.542 ms | 0.058 ms (11%) | 0.484 ms (89%) | 0.552 ms |
| 50 | 0.283 ms | 0.022 ms (8%) | 0.261 ms (92%) | 0.320 ms |

### Through the gateway

The same run with the bench connected to the gateway's `/ws` proxy
(`ws://localhost:8080`) instead of the websocket service. One run per row.
Production does not take this hop: the ingress routes `/ws` straight to the
websocket service.

| Pairs | p50 ms | p95 ms | p99 ms | vs direct p50 |
|---|---|---|---|---|
| 1 | 1.997 | 2.534 | 3.079 | +0.702 |
| 10 | 0.660 | 1.334 | 2.145 | +0.118 |
| 50 | 0.344 | 0.462 | 0.653 | +0.061 |

### Reading these numbers

- **The server holds a message for 0.02–0.13 ms on average.** About nine tenths
  of the local end-to-end time is outside it: Docker Desktop's port forwarding,
  the loopback, and the bench process itself.
- **More pairs is faster, not slower.** One pair sends 20 messages a second, so
  each message finds the path idle: goroutines parked, the VM's network path
  asleep. Every stage pays a wake-up, the server's included (0.131 ms at one
  pair, 0.022 ms at fifty, for the same work). At 50 pairs (1 000 messages a
  second) everything stays warm. The one-pair row measures wake-up latency more
  than work; a real client, idle between packets, sees the same effect.
- **Noise floor.** At one pair the three runs' p50 spans 0.33 ms; at 50 pairs,
  0.02 ms. A change must move a number by more than its row's span to count.
  The 50-pair row is the one that can resolve small differences.
- **The tail.** The slowest message of a run takes 2.7–7.6 ms at every load,
  which at 50 pairs is about ten times p99. The likely cause is a garbage
  collection pause or a scheduling delay, in the server, the bench process or
  the Docker VM. It was recorded, not investigated.

## Changes

Measured 2026-11-12 on the same machine, after Docker Desktop had been
restarted. The restart alone moved the numbers (one pair: p50 1.071 ms against
the baseline's 1.295 ms, on the same code), so every change is compared with a
control taken in the same session, never with the baseline table. Each change
gets three runs at 50 pairs and counts only if it moves p50 outside the
control's range.

No change to the server made the relay faster, and none is in the code. The
server's share was 0.02 ms of 0.29 ms to begin with.

| Item | What was tried | 50 pairs p50 | Against control | Outcome |
|---|---|---|---|---|
| Control | Nothing | 0.290 ms (0.275–0.296) | | |
<!-- summary rows -->

### Control

Commit `afbff13`, direct to the websocket service. Median of three runs, range
in brackets.

| Pairs | p50 ms | p95 ms | p99 ms | Server total (mean) |
|---|---|---|---|---|
| 1 | 1.071 (0.987–1.186) | 1.578 (1.461–1.596) | 1.949 (1.800–2.004) | 0.108 ms |
| 10 | 0.473 (0.461–0.485) | 0.903 (0.882–0.969) | 1.275 (1.183–1.285) | 0.053 ms |
| 50 | 0.290 (0.277–0.293) | 0.426 (0.402–0.428) | 0.590 (0.573–0.628) | 0.024 ms |
| 50, repeated after B2 | 0.290 (0.275–0.296) | 0.389 (0.369–0.400) | 0.544 (0.541–0.601) | 0.022 ms |

The six 50-pair control runs span 0.275–0.296 ms at p50 and 0.369–0.428 ms at
p95. That is the noise floor: 0.02 ms at p50.

## Message classes

| Class | Messages | Path | Guarantee |
|---|---|---|---|
| Ephemeral | `relay` (movement, position, input) | client → hub → clients, in memory | At-most-once; shed for a slow receiver; one replica only |
| Ephemeral | `PlayerJoined`, `PlayerLeft` (room notices) | hub → clients | Same |
| Durable | `MatchFound`, `LeaderboardUpdated`, presence | service → JetStream → websocket bridge | At-least-once |
| Durable | `PlayerRegistered`, `PlayerUpdated`, `SessionRevoked` | Postgres outbox → relay → JetStream | Never lost once committed |

Anything that must not be lost (a match result, a score) goes through the REST
API and the durable path, never through `relay`.

## Not done

- **Cross-replica relay.** The relay fans out from one replica's hub, so two
  players of a room on different websocket replicas never receive each other's
  messages. Production runs one replica and its manifest says why
  (`deployments/k8s/overlays/prod/kustomization.yaml`); the local k3d overlay
  runs two and is broken for the relay. The fix is an ephemeral fan-out between
  replicas, Redis Pub/Sub or core NATS (not JetStream): each replica publishes
  a relay message once and delivers what it receives to its own room members.
  It adds a hop for cross-replica pairs and was left out of this work.

## Running it

```sh
go run ./scripts/latencybench -pairs 10 \
  -metrics http://localhost:8084/metrics -out run.json \
  -cleanup-dsn 'postgres://gamemesh:gamemesh-dev-password@localhost:5432/gamemesh?sslmode=disable'
```

`-pairs` takes 1, 10 or 50; `-ws ws://localhost:8080` goes through the gateway.

Bench accounts are `bench<run><n>` with the address
`bench_<run>_<n>@latencybench.invalid` (usernames must be alphanumeric, so the
address carries the marker; `.invalid` is a reserved TLD). Every run leaves the
matchmaking queue and logs its players out.

There is no account-deletion endpoint, so removing the rows needs the
database, and only happens when `-cleanup-dsn` is given (it has no default). A
player is deleted only if both hold: the address matches
`%@latencybench.invalid` and the username starts with `bench`. The tool counts
the matching players and prints the number first (`-cleanup-dry-run` stops
there), deletes at most `-cleanup-limit` players (200) with their outbox rows,
and reports what it deleted and what remains. `-cleanup-only` does just this,
for a run that crashed.

Against production the tool is run without `-cleanup-dsn`: the bench accounts
stay, recognisable by the same two marks.

What a run leaves outside Postgres expires on its own: the matchmaking room in
Redis (1 h), and the `PlayerRegistered`, `SessionRevoked` and `MatchFound`
events in JetStream (72 h, 72 h, 24 h).
