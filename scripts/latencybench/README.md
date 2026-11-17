# latencybench

Measures how long one player's WebSocket message takes to reach the other
player of the room. What it measures, how, and every result:
[docs/latency.md](../../docs/latency.md).

It registers 2 × N players, pairs them through matchmaking, joins each pair
into its room and sends 550 messages from one to the other, one every 50 ms
(the first 50 are warm-up). Both ends are in this process, so the time is taken
on one monotonic clock.

## Against the local stack

```sh
docker compose up -d
go run ./scripts/latencybench -pairs 10 \
  -metrics http://localhost:8084/metrics -out run.json \
  -cleanup-dsn 'postgres://gamemesh:gamemesh-dev-password@localhost:5432/gamemesh?sslmode=disable'
```

- `-pairs`: concurrent pairs; the documented runs used 1, 10 and 50.
- `-metrics`: the websocket service's `/metrics`, for the server's own stage
  times over the same window. Without it only the client side is reported.
- `-ws ws://localhost:8080`: go through the gateway instead of straight to the
  websocket service.
- `-out`: the full result as JSON. `go run ./scripts/latencybench -h` lists the
  rest.

## Ghost Tag: the validated room (local only)

`-mode ghosttag` measures the same thing through a Ghost Tag room, where the
server opens each position, validates it and sends its own `pos` to the other
player. Run it against the local stack only, **never production**: the tool
refuses an `-api`, `-ws` or `-metrics` that is not localhost (design doc
section 9, Note B).

```sh
docker compose up -d
go run ./scripts/latencybench -mode ghosttag -pairs 10 \
  -metrics http://localhost:8084/metrics -out ghosttag.json \
  -cleanup-dsn 'postgres://gamemesh:gamemesh-dev-password@localhost:5432/gamemesh?sslmode=disable'
```

What changes from the relay mode:

- Players queue with `"mode":"ghosttag","unranked":true`, always; a server that
  does not confirm the ticket as unranked fails the run. Bench matches never
  score.
- A pair whose ticket waited 5 s alone gets a room with the server's bot. The
  run fails rather than measure against it.
- After the join the bench waits for the first half (`HALF_1_ACTIVE`, after a
  2 s countdown). The server drops `seq` and `cts`, so nothing the client sends
  survives but the position: the sender moves from its spawn along +y, 0.5
  units per message, and the receiver recovers each message's index from the
  `y` it is told. Send times stay in the process, on one monotonic clock.
- `clipped` counts positions that map to no message sent: the server
  corrected them. It must be 0; anything else fails the run.
- `-interval` defaults to 40 ms (the game's 25 Hz). `-samples` × `-interval`
  must end before the arena starts to shrink, 30 s into the half (with 1 s
  margin), and the path must stay inside the arena: 550 × 40 ms is 22 s and
  275 units. The tool refuses anything else up front.
- `-metrics` reads `gamemesh_ws_game_duration_seconds` instead of the relay's
  histogram, with `gamemesh_ws_game_dropped_total`,
  `gamemesh_ws_relay_rejected_total` and `gamemesh_ws_game_events_total`.
- `-payload` does not apply: the sender sends `pos` messages (about 65 bytes).

The rooms need no cleanup: once the players leave, a room goes to its grace
and ends unranked.

## Against production

```sh
caffeinate -i go run ./scripts/latencybench -pairs 1 \
  -api https://api.gamemesh.app -ws wss://api.gamemesh.app -out prod-1.json
```

Relay mode only (the default). No `-cleanup-dsn` and no `-metrics`: production's database is not reachable and
is not to be reached for this, and `/metrics` is not public. The accounts the
run creates stay. `caffeinate` because a laptop that sleeps mid-run loses the
run. Keep it to 1 and 10 pairs: this is the live server, and setup's logins
are bcrypt-bound.

## The accounts it creates

`bench<run><n>` with the address `bench_<run>_<n>@latencybench.invalid`. Every
run closes its sockets, leaves the matchmaking queue and logs its players out.

Rows are deleted only with `-cleanup-dsn` (it has no default), and only for
players that match both marks: the address `%@latencybench.invalid` and a
username starting with `bench`.

```sh
# count what an earlier run left behind, delete nothing
go run ./scripts/latencybench -cleanup-only -cleanup-dry-run -cleanup-dsn '…'
# delete them, at most 200 per call (-cleanup-limit)
go run ./scripts/latencybench -cleanup-only -cleanup-dsn '…'
```
