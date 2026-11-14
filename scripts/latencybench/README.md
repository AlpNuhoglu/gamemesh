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

## Against production

```sh
caffeinate -i go run ./scripts/latencybench -pairs 1 \
  -api https://api.gamemesh.app -ws wss://api.gamemesh.app -out prod-1.json
```

No `-cleanup-dsn` and no `-metrics`: production's database is not reachable and
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
