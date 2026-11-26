# In-game latency

How long one player's message takes to reach the other player over WebSocket,
how that time splits between the server and everything around it, and what each
change to the server did to it. Updated after every change.

The load-test figures in the README (matchmaking p95 27–31 ms at 500 virtual
users, and the others in that table) are HTTP request latency under load. They
say nothing about this path.

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

## Production

The number to quote. Measured 2026-11-12 on commit `1554985`, from a laptop on
a home connection in İstanbul to `wss://api.gamemesh.app`, a netcup VPS in
Nuremberg: TLS, Traefik, then one websocket replica on the single-node k3s
cluster (2 vCPU, 4 GB). One run per row, 550 messages per pair, the first 50
discarded.

The 40 ms belongs to that pair of places. A TCP connect from the same laptop to
the same data centre took 42–46 ms when the server was chosen (2026-10-20,
`docs/deployment-phase1-plan.md`): one round trip, the same distance. A third
run two days later was slower, on the network side
([below](#a-third-run-two-days-later)).

| Pairs | Samples | Lost | p50 ms | p95 ms | p99 ms | max ms | Ping RTT p50 | Ping RTT p95 |
|---|---|---|---|---|---|---|---|---|
| 1 | 500 | 0 | 39.867 | 41.705 | 42.826 | 47.110 | 39.963 ms | 41.685 ms |
| 10 | 5 000 | 0 | 41.691 | 44.995 | 47.987 | 138.034 | 41.564 ms | 44.126 ms |

- **It is all network.** End-to-end p50 and the ping round trip differ by
  0.10 ms and 0.13 ms, in opposite directions. The server held each message
  for 0.15 ms and 0.12 ms on average (below), under half a percent of these
  40 ms.
- **The 40 ms is this client's path, twice.** A message goes sender → server →
  receiver, and both ends were the same laptop. Two real players each bring
  their own path to the server.
- The two rows were measured ten hours apart (01:14 and 11:42 UTC): the first
  attempt at ten pairs was lost when the laptop went to sleep mid-run. They
  should not be read as "ten pairs is 1.8 ms slower than one".
- **The 138 ms maximum at ten pairs is one message, and it was not the
  server:** no message spent more than 2.5 ms inside it in that window (below).
  It was recorded, not traced.
- 50 pairs were not run against production.
- The bench ran without `-cleanup-dsn`. 42 bench accounts remain in production
  (2, 20 from the lost run, 20), recognisable by `bench…@latencybench.invalid`.

### A third run, two days later

Made on 2026-11-14 at 19:29 UTC (22:29 in İstanbul, a Saturday) on commit
`ce16c44`, from the same laptop: one pair, to put traffic on the status page's
relay panels for the picture in the README. It was not meant as a measurement.
It is recorded because it does not look like the first two.

| Pairs | Samples | Lost | p50 ms | p95 ms | p99 ms | max ms | Ping RTT p50 | Ping RTT p95 |
|---|---|---|---|---|---|---|---|---|
| 1 | 500 | 0 | 73.134 | 117.193 | 391.195 | 541.095 | 65.837 ms | 130.462 ms |

- **The network round trip was longer and less even.** The ping on the run's
  own two sockets came back in 66 ms at p50 and 130 ms at p95, against 40 ms
  and 42 ms two days earlier. It is 40 pings, so a coarse figure.
- **The server was not the difference.** It held each message for 0.20 ms on
  average and none for as long as 5 ms (below): 0.28 % of the 73 ms, and
  nothing near the 541 ms maximum.
- Why the path was slower that evening was not looked into: the hour, the
  connection and the laptop itself were not told apart.
- **So 40 ms is what this path gave twice, not a floor it holds.** The client's
  connection is part of the number.
- Two more bench accounts stay in production: 44 in all.

### Inside the server, in production

`/metrics` is not public, so the bench could not scrape the stage histogram
the way it does locally. These are the same counters read afterwards from
production's Prometheus, as the difference across each run's window
(01:13:45–01:15:00 and 11:42:15–11:43:30 UTC on 2026-11-12, 19:29:30–19:31:30
UTC on 2026-11-14). The counts in each window are exactly the run's messages,
warm-up included (550, 5 500 and 550).

| Pairs | Messages | process mean | queue mean | write mean | total mean | total ~p95 | total ~p99 | Slower than 2.5 ms | Shed | Rejected |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | 550 | 0.065 ms | 0.036 ms | 0.068 ms | 0.148 ms | 0.249 ms | 0.480 ms | 0 | 0 | 0 |
| 10 | 5 500 | 0.050 ms | 0.027 ms | 0.056 ms | 0.116 ms | 0.243 ms | 0.428 ms | 0 | 0 | 0 |
| 1, third run | 550 | 0.089 ms | 0.060 ms | 0.086 ms | 0.203 ms | 0.458 ms | 1.375 ms | 1 | 0 | 0 |

Means are exact (Δsum/Δcount); p95 and p99 are interpolated within histogram
buckets, as in the local tables.

- **0.37 %, 0.28 % and 0.28 % of the end-to-end p50** (0.148 of 39.867 ms,
  0.116 of 41.691 ms, 0.203 of 73.134 ms).
- **More than locally at the same loads** (0.11–0.13 ms at one pair and
  0.05–0.06 ms at ten, on a 14-core laptop against two vCPUs here). Why was
  not looked into. Across everything measured, local and production, the
  server's mean share is 0.02–0.20 ms.
- **The third run is the slowest of the three inside the server,** and the one
  with a message over 2.5 ms (one of 550, under 5 ms). The websocket pod had
  started three minutes before it (19:26:54 UTC, the deploy of `ce16c44`) and
  these were the first messages it relayed. Whether that is the reason was not
  looked into.

## Baseline

Measured 2026-11-11 on commit `2b233e8`, before any latency change.

**Environment.** Bench and server on the same machine: Apple M4 Pro (14 cores),
macOS 27.0.1, Docker Desktop 29.9.2 (Linux VM, 14 CPUs, 8 GB). The server is
the `docker compose` stack, one websocket container; the bench runs natively on
macOS and reaches it at `ws://localhost:8084`, so every message crosses Docker
Desktop's port forwarding twice. There is no real network here: these numbers
describe the server and the local plumbing, not what a player sees; for that,
see [Production](#production).

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
| B1 | Boundary tests; no runtime change | 0.290 ms (the control is this commit) | 0 ms | Tests kept |
| B2 | Explicit `SetNoDelay(true)` | 0.293 ms (0.286–0.297) | +0.003 ms, inside the noise | Reverted; test kept |
| B3 | Nothing: compression is not negotiated | | | Test kept |
| B4 | Nothing: no batching on the path | | | |
| B5 | Nothing in production; measured the local gateway hop | 0.333 ms (0.329–0.339) through the gateway | +0.043 ms for the hop | Production already direct |
| B6 | Nothing: no wait in the write path | | | |

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

### B1. Keep game traffic off the durable path: 0 ms, boundary pinned

No game message ever went through JetStream or the outbox. The relay was built
hub-direct, so there was nothing to move and nothing gained. What B1 leaves is
the boundary as four checks on the source
(`internal/wsgateway/boundary_test.go`):

- `relay.go` imports neither the event bus, the outbox nor a transport;
- only `bridge.go`, the consumer, imports the event bus;
- the WebSocket service contains no `Publish` call, so nothing a client sends
  can be put on the bus;
- the relay's frame type is not the name of a durable event.

Run unchanged against the experiment below, three of the four failed. The
first passed, because the experiment put its publish in a new file and that
check reads only `relay.go`; the second and third are the ones that catch a
reroute.

#### EXPERIMENT: the same message through JetStream

This is not a before and after. The relay never took this route, and nothing
was made faster by not taking it. It is a one-off measurement of what the
durable route would cost for the same message, on branch
[`exp/relay-jetstream`](https://github.com/AlpNuhoglu/gamemesh/tree/exp/relay-jetstream)
(`04064a2`). The branch is kept so the number can be reproduced; it is not
merged and must not be: three of the boundary tests are skipped on it.

In that build an accepted relay message is published to a JetStream stream of
its own the way backend events are (synchronous publish, waiting for the
JetStream ack; file storage), consumed back by the websocket service's own
bridge consumer and its worker pool, and only then queued for the room.

| Pairs | Direct p50 | JetStream p50 | Difference | Direct p99 | JetStream p99 | Server total, direct | Server total, JetStream |
|---|---|---|---|---|---|---|---|
| 1 | 1.071 ms | 2.737 ms (1.795–2.824) | +1.666 ms | 1.949 ms | 4.627 ms | 0.108 ms | 1.074 ms |
| 10 | 0.473 ms | 0.812 ms (0.708–0.816) | +0.339 ms | 1.275 ms | 2.861 ms | 0.053 ms | 0.365 ms |
| 50 | 0.290 ms | 0.382 ms (0.375–0.388) | +0.092 ms | 0.590 ms | 0.873 ms | 0.024 ms | 0.144 ms |

Three runs per row, same session as the control; no run lost or reordered a
message.

- The server holds the message six to ten times longer. Most of it is the
  publish: the sender's read loop waits 0.12–0.83 ms for the ack before it can
  read the next message.
- This is JetStream at its best: NATS on the same host, one stream replica, an
  idle stream. A slow or unreachable NATS would hold every relay for up to the
  5 s publish timeout, where the direct path does not depend on it at all.
- The outbox route (a Postgres insert, then the relay's poll) was not measured.

### B2. Nagle: already off, explicit call reverted (0 ms)

Checked first, on the socket: `TCP_NODELAY`, read with `getsockopt` from the
server's side of a real upgraded connection, is on. On macOS and on Linux, in
the Go 1.26.11 image the service is built from. Go sets it on every TCP socket
it creates.

Then measured anyway, with an explicit `SetNoDelay(true)` after the upgrade:

| | p50 ms | p95 ms | p99 ms | Server total (mean) |
|---|---|---|---|---|
| Control, before | 0.290 (0.277–0.293) | 0.426 | 0.590 | 0.024 ms |
| Explicit `SetNoDelay(true)` | 0.293 (0.286–0.297) | 0.408 | 0.559 | 0.023 ms |
| Control, after | 0.290 (0.275–0.296) | 0.389 | 0.544 | 0.022 ms |

No difference, so the call was not committed. What is kept is the check
(`nodelay_test.go`), which fails if the option is ever off; it was seen to fail
with `SetNoDelay(false)`. Not checked: the sockets of Traefik, which sits in
front of the service in production.

### B3. Compression: already off (no change)

The upgrader never sets `EnableCompression`, and gorilla negotiates
`permessage-deflate` only when it is set. Checked on the wire: a client that
offers the extension gets a handshake without `Sec-WebSocket-Extensions`
(`compression_test.go`, seen to fail with compression enabled). Writing
`EnableCompression: false` would produce the same binary, so there was nothing
to measure.

### B4. Batching: none on this path (no change)

The batching in this codebase is in the outbox relay's poll, the matchmaking
tick and the presence heartbeat. The WebSocket path has none: a relay frame
goes from the fan-out into the receiver's send buffer and is written by the
next wake-up of its writer. The `queue` stage shows it: 0.006 ms mean and
0.022 ms p99 at 50 pairs. A batch window, even the 16 ms one, would appear
there.

### B5. Extra hop: production has none; the gateway hop costs 0.04 ms (no change)

In production the ingress routes `/ws` straight to the websocket service. The
gateway's `/ws` proxy is reachable only locally, and going through it costs:

| 50 pairs | p50 ms | p95 ms | p99 ms |
|---|---|---|---|
| Direct (control) | 0.290 (0.277–0.293) | 0.426 | 0.590 |
| Through the gateway | 0.333 (0.329–0.339) | 0.489 | 0.664 |

+0.043 ms at p50, outside the noise. The route was left in place: it is not on
the production path, and local clients that care connect to `:8084`, as the
bench and the k6 script already do.

### B6. Write path: no wait found (no change)

The writer's only ticker is the 54 s ping. Each message is one `WriteMessage`,
which writes one frame to the socket at once; a relay frame is under 300 bytes
and the write buffer is 1024. Measured at 50 pairs: `write` 0.013 ms mean and
0.035 ms p99, `queue` 0.006 ms. End-to-end p50 (0.290 ms) is no more than the
ping round trip (0.32 ms), so nothing waits after the write either.

Seen and left alone, since the whole server share is 0.024 ms: the fan-out
takes the hub's read lock, which joins and disconnects hold for writing; and
the read side allocates for every message.

## Message classes

| Class | Messages | Path | Guarantee |
|---|---|---|---|
| Ephemeral | `relay` in an opaque-relay room (movement, position, input) | client → hub → clients, in memory, unread | At-most-once; shed for a slow receiver; one replica only |
| Ephemeral | `relay` in a validated room (Ghost Tag `pos`, `ping`), and the room's answers as `Game` frames | client → hub → the room's game → clients, in memory | Same |
| Ephemeral | `PlayerJoined`, `PlayerLeft`, `JoinRejected` (room notices) | hub → clients | Same |
| Durable | `MatchFound`, `LeaderboardUpdated`, presence | service → JetStream → websocket bridge | At-least-once |
| Durable | `PlayerRegistered`, `PlayerUpdated`, `SessionRevoked` | Postgres outbox → relay → JetStream | Never lost once committed |

Anything that must not be lost goes through a durable path, never through
`relay`. Until Ghost Tag that meant the REST API. A validated room produces its
match result on the server, and a ranked result is written to Redis by the
WebSocket service in one transaction keyed by the room (design doc, section 4);
what reaches the players about it (`match_end`) is still an ephemeral `Game`
frame. That store is not built yet: every match is unranked so far.

## Validated rooms (Ghost Tag), measured 2026-11-17

Ghost Tag adds a second mode to the room. An **opaque-relay** room is what the
sections above measure: the server passes the data on unread. In a
**validated room** the server opens each position packet, validates it
(speed, turn, arena), checks for a capture, and broadcasts a `pos` of its own;
the client's packet never reaches the other player (`internal/wsgateway/game.go`,
`internal/ghosttag`). The design rule (Ghost Tag design doc, section 9, Note B) is
that what this costs is measured, not assumed. Three measurements answer it.

All runs: local stack (`docker compose`), Docker Desktop on an M4 Pro,
10 pairs, 500 counted messages per pair, every 50 ms, the same session.
`scripts/latencybench -mode ghosttag` drives a validated room (it is local
only and refuses any non-loopback address); its matches are unranked.

### 1. The opaque relay, before and after this work

Adding validated rooms changed the relay's own path in one place: the frame is
now built under the hub's read lock, after a lookup that the room is not a
validated room. The WebSocket container was switched between the image built
at `b58b893` (before) and at the Ghost Tag branch (after), the rest of the
stack unchanged, in the order before, after, before, after, before, after.
Payload 120 bytes, as in the earlier runs.

| Run | Image | End-to-end p50 | p95 | Server `process` mean (exact) | `process` p95 (estimate) | Server `total` mean |
|---|---|---|---|---|---|---|
| 1 | before | 0.546 ms | 1.154 ms | 0.025 ms | 0.053 ms | 0.060 ms |
| 2 | after | 0.543 ms | 1.194 ms | 0.025 ms | 0.061 ms | 0.062 ms |
| 3 | before | 0.542 ms | 1.282 ms | 0.024 ms | 0.050 ms | 0.060 ms |
| 4 | after | 0.563 ms | 1.262 ms | 0.026 ms | 0.063 ms | 0.063 ms |
| 5 | before | 0.562 ms | 1.266 ms | 0.025 ms | 0.057 ms | 0.064 ms |
| 6 | after | 0.558 ms | 1.364 ms | 0.026 ms | 0.061 ms | 0.062 ms |

End to end there is no difference: the before and after p50s overlap
(0.542–0.562 ms against 0.543–0.563 ms). Inside the server there is a small,
consistent one: the exact `process` mean goes from 0.0247 to 0.0257 ms on
average (+1 µs), and the `process` p95 of every "after" run is above that of
every "before" run (about +8 µs; a bucket estimate, the buckets there are 25,
50 and 100 µs). The README's 0.02–0.20 ms for the server's share still holds.

### 2. Opaque relay against validated room

The same image (after), alternating the two modes: relay, validated, relay,
validated, relay, validated. To compare the path and nothing else, both send
every 50 ms and the relay's payload is cut to 65 bytes, the size of a Ghost
Tag `pos`. Every validated run: 0 lost, 0 positions corrected by the server
(`clipped=0`), no warning.

| Run | Mode | End-to-end p50 | p95 | `process` mean (exact) | `process` p95 (estimate) | `queue` mean | `total` mean |
|---|---|---|---|---|---|---|---|
| 1 | relay | 0.583 ms | 1.453 ms | 0.028 ms | 0.074 ms | 0.017 ms | 0.068 ms |
| 2 | validated | 0.540 ms | 1.121 ms | 0.038 ms | 0.093 ms | 0.007 ms | 0.073 ms |
| 3 | relay | 0.538 ms | 1.194 ms | 0.024 ms | 0.060 ms | 0.015 ms | 0.059 ms |
| 4 | validated | 0.534 ms | 1.122 ms | 0.037 ms | 0.092 ms | 0.007 ms | 0.071 ms |
| 5 | relay | 0.505 ms | 1.090 ms | 0.023 ms | 0.049 ms | 0.015 ms | 0.057 ms |
| 6 | validated | 0.530 ms | 1.106 ms | 0.039 ms | 0.094 ms | 0.008 ms | 0.075 ms |

The relay's stages are read from `gamemesh_ws_relay_duration_seconds`, the
validated room's from `gamemesh_ws_game_duration_seconds`; both have the same
four stages, measured at the same points (read from the socket, queued for the
receiver, written).

- **`process` is about 13 µs longer** in a validated room: 0.038 ms on average
  against 0.025 ms. It is the stage that does the extra work: the packet is
  handed to the room's goroutine, decoded, validated, written to the ring,
  checked for a capture, and a new `pos` is encoded.
- **`queue` is about 8 µs shorter**, and the server's `total` ends up 12 µs
  longer: 0.073 ms against 0.061 ms. Why `queue` is shorter was not
  investigated; it is reported as measured.
- **End to end there is no difference** at this resolution: 0.530–0.540 ms
  against 0.505–0.583 ms. The 12 µs is 0.03 % of the 40 ms a message takes in
  production.

### 3. The per-packet work alone

`go test -bench` on the Ghost Tag package, ten runs each, same machine:

| Benchmark | What it covers | Median | Range | Allocations |
|---|---|---|---|---|
| `BenchmarkPacketCore` | validate + ring write + capture check | 75.2 ns | 74.6–78.8 ns | 0 |
| `BenchmarkHandlePos` | the room's whole handling of a decoded packet, including encoding the outbound `pos` | 239.5 ns | 234.7–247.8 ns | 1 (126 B) |
| `BenchmarkDecodePos` | decoding the inbound JSON | 478.8 ns | 474.7–483.3 ns | 1 (64 B) |

The validation itself is under 0.1 µs, and decoding plus handling under 1 µs.
Against the 13 µs that `process` grew by, this says the time is not in the
game's arithmetic. The rest is most likely the hand-off from the connection's
goroutine to the room's through a channel; that is an inference from the two
measurements, not a third one.

### Reading it

Validation is not free inside the server, and it is not visible outside it.
Its cost is about 12 µs per message, against 0.06 ms the server spent on the
relay and 40 ms end to end in production. Nothing was optimised: the
measurement is the result.

## In a real browser (Ghost Tag client), measured 2026-11-26

Everything above was measured with `scripts/latencybench`, a Go client. A
browser is a different client: it applies input once per frame, sends its
position on a 40 ms timer, and draws the other player through an
interpolation buffer. The design rule for the client (Ghost Tag design doc,
section 9, Note B) is that this is measured in a real browser as soon as the
client runs: from a key going down in one player's page to the frame in which
the other player's page draws the movement.

Setup: the local stack (`docker compose`, Docker Desktop on an M4 Pro, macOS
27.0.1), the built client as the gateway serves it under `/play/`, and the
installed Google Chrome 158, headless, driven by Playwright. Two pages in two
browser contexts (two guest accounts) play each other. Both pages measured a
frame interval of 16.67 ms (60 Hz). Everything runs on one machine over
loopback, so there is no network in these numbers.

Method: the page being watched is at rest. A key is held for 200 ms in it.
Both pages stamp events with `performance.timeOrigin + performance.now()`,
the same wall clock on one machine:

| Stamp | Taken |
|---|---|
| key | the `keydown` event's own timestamp, in the pressing page |
| applied | the frame that moved the pressing page's own character |
| sent | the first position packet that carries the movement |
| drawn | each frame in which the watching page drew the opponent at a new place |

All but the first come from the client's developer probe (`?debug=1`); nothing
else in the client depends on it.

### From the key to the first frame that shows it

The runner presses, the hunter watches (the hunter sees the runner live). 71
presses over the two halves of one match; one more was left out because the
server corrected the character in the same moment.

| Stage (ms) | min | p50 | p95 | max |
|---|---|---|---|---|
| key → applied | 0.2 | 7.8 | 15.8 | 16.3 |
| applied → sent | 0.1 | 9.3 | 31.9 | 34.7 |
| sent → first drawn | 0.1 | 30.3 | 37.0 | 37.6 |
| **key → first drawn** | 14.9 | **49.0** | **70.4** | 77.3 |

The stages are what the client's design predicts: up to one frame before the
input is applied, up to one send interval (40 ms) before a packet carries it,
and then the buffer. "First drawn" is the first frame in which the opponent is
anywhere new, not the frame in which it has arrived: the watching page draws
60 ms behind and interpolates between packets 40 ms apart, so the picture
starts to move about 20 ms after the packet lands, plus the wait for a frame.

### How old the picture is while the opponent moves

The first frame says when movement becomes visible. How far behind the
picture then stays is the interpolation buffer, and that is the number the
buffer's setting can be checked against. For every frame the watching page
drew during a press, the position drawn was matched to the moment the
pressing page had sent that position (its packets are 40 ms apart and the
character moves in a straight line between two of them).

| View | Frames | p5 | p50 | p95 | Setting |
|---|---|---|---|---|---|
| Live: the hunter watching the runner | 935 | 60.1 ms | **60.9 ms** | 65.2 ms | `interp_ms` 60 |
| Trail: the runner watching the hunter | 387 | 2000.9 ms | **2001.9 ms** | 2002.9 ms | `trail_ms` 2000 |

The trail was measured in a second match, with the hunter pressing: 26
presses, every frame between 2000.4 and 2017.9 ms behind.

Both views are behind by their setting plus one to two milliseconds, which is
what the packet takes through the server on loopback. So the interpolation
buffer and the trail delay do what `snap.cfg` tells them to, and nothing else
in the path holds a position back.

In the live view 891 of the 935 frames were between 55 and 70 ms behind. The
other 44 were in 19 of the 71 presses; in two of them a run of frames fell
steadily further behind (135 to 273 ms in one, 181 to 225 ms in the other).
That is what a stalled page or a late packet would look like, and also what a
wrong match of position to packet would; it was not traced.

### Reading it

- On one machine a key press is visible on the other player's screen after
  about 49 ms (p50) and 70 ms (p95), and the picture of a moving opponent is
  61 ms old. In production the network comes on top, once: the end-to-end
  figures of the "Production" section above.
- For the runner the hunter is 2.0 seconds old by design. The 60 ms of the
  live view do not appear there on top: the trail is taken from its own
  buffer.
- Nothing was tuned. The send interval and the two delays are the server's
  `Config`; changing one changes these numbers by as much.

Limits of the measurement:

- "Drawn" is the frame's `requestAnimationFrame` callback. The compositor and
  the display come after it and cannot be measured without a camera; they are
  not in these numbers.
- Headless Chrome, one browser, one machine that also runs the server and the
  second page. A real display, another browser or a phone were not measured.
- One match for each view. The keyboard only: the mouse and touch paths were
  not timed.

To repeat it, against a local stack whose gateway serves the built client:

```sh
cd web && E2E_BASE_URL=http://127.0.0.1:8080 npm run latency
```

It takes about three and a half minutes and writes every sample to
`web/bench-out/latency.json`. It stops at the start if the pages are not
drawing at 60 Hz. That happened once: on the same day, with the laptop at 19 %
battery, every page of the browser (a blank one included) drew at 33.3 ms a
frame. A run taken then, of a later build of the client (the visual pass),
put the first frame at 59 ms (p50) instead of 49. Two things differed in that
run, the frame rate and the build, so it says nothing about either alone; the
later build has to be measured again at 60 Hz. The two buffers measured the
same in it (60.9 and 2001.8 ms), as they should: they do not depend on the
frame rate.

## Not done

- **Cross-replica relay.** The relay fans out from one replica's hub, so two
  players of a room on different websocket replicas never receive each other's
  messages. Production runs one replica and its manifest says why
  (`deployments/k8s/overlays/prod/kustomization.yaml`); the local k3d overlay
  runs two and is broken for the relay. The fix is an ephemeral fan-out between
  replicas, Redis Pub/Sub or core NATS (not JetStream): each replica publishes
  a relay message once and delivers what it receives to its own room members.
  It adds a hop for cross-replica pairs and was left out of this work.
- **Deleting an account.** When these measurements were taken there was no
  endpoint for it (decided 2026-11-13), which is why the bench cleans up
  through the database and left its accounts behind in production. Since the
  browser client step there is one, behind the release flag
  (`DELETE /api/v1/players/me`), and the account sweep removes what the bench
  left once the flag is on (Ghost Tag design doc, section 9, Note C).
- **The slow tail.** The 3–8 ms maximum at every load was recorded, not traced.
- **`WS_RELAY_RATE=0`.** It is accepted and rejects every relay message; the
  setting is not validated.
- **Lock and allocations.** The relay fan-out shares the hub's lock with joins
  and disconnects, and the read side allocates per message. The server's whole
  share is 0.02 ms at 50 pairs, so neither was touched.

## Since the measurement

Done after these numbers were taken (#133, 2026-11-14), and not re-measured:

- **Who may join a room.** `join` used to accept any room ID from any
  authenticated client. It is now checked against the two players matchmaking
  stored for the room (`room:{id}` in Redis), and refused with `JoinRejected`
  otherwise. That is one Redis read per join; a relay message does not pay it.
- **The rate limit is per player**, not per connection: one bucket for all of a
  player's connections, shared with joins.

Both are outside the relay's path once a player is in the room, so the numbers
above should stand. That was an expectation; the measurement of 2026-11-17
("Validated rooms" below) took the relay before and after the later change to
its own path and found the server's share within 1 µs on the mean.

## For the game client (phase 3)

Two requirements for the client that phase 3 builds, decided 2026-11-13. They
follow from limits that are staying as they are.

- **One combined message, at most 30 a second.** A client sends what the other
  side needs (position, input, state) as one relay message per tick, not as a
  stream per kind. A player's bucket is 60 messages a second with a burst of
  120, across all of the player's connections and including joins
  (`WS_RELAY_RATE`, `WS_RELAY_BURST`); the data of one message is at most
  1 024 bytes. Two 30 Hz streams would use the whole allowance and leave
  nothing for a join, a second tab or the burst after a stall; one stream
  leaves half of it free. Messages over the limit are dropped and counted,
  without a reply.
- **Guest sessions, no real e-mail address.** The demo client plays under a
  guest or anonymous session, which phase 3 has to add. With no way to delete
  an account, the public demo must not collect an address that somebody may
  want removed.

## Running it

```sh
go run ./scripts/latencybench -pairs 10 \
  -metrics http://localhost:8084/metrics -out run.json \
  -cleanup-dsn 'postgres://gamemesh:gamemesh-dev-password@localhost:5432/gamemesh?sslmode=disable'
```

`-pairs` is the number of concurrent pairs (the runs here used 1, 10 and 50);
`-ws ws://localhost:8080` goes through the gateway. Against production:
[scripts/latencybench/README.md](../scripts/latencybench/README.md).

Bench accounts are `bench<run><n>` with the address
`bench_<run>_<n>@latencybench.invalid` (usernames must be alphanumeric, so the
address carries the marker; `.invalid` is a reserved TLD). Every run leaves the
matchmaking queue and logs its players out.

Removing the rows through the database only happens when `-cleanup-dsn` is
given (it has no default). A
player is deleted only if both hold: the address matches
`%@latencybench.invalid` and the username starts with `bench`. The tool counts
the matching players and prints the number first (`-cleanup-dry-run` stops
there), deletes at most `-cleanup-limit` players (200) with their outbox rows,
and reports what it deleted and what remains. `-cleanup-only` does just this,
for a run that crashed.

Against production the tool is run without `-cleanup-dsn`. At the end of a run
it deletes its accounts through the API (`DELETE /api/v1/players/me`) when the
server offers that endpoint. When it does not (a 404), the accounts stay,
recognisable by the same two marks, and are removed by the server's account
sweep once that is on.

What a run leaves outside Postgres expires on its own: the matchmaking room in
Redis (1 h), and the `PlayerRegistered`, `SessionRevoked` and `MatchFound`
events in JetStream (72 h, 72 h, 24 h).
