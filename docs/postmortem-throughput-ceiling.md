# Post-Mortem: Matchmaking Throughput Ceiling Under High-Concurrency Load

| Field | Value |
|---|---|
| **Document ID** | PM-2026-014 |
| **Title** | Superlinear p95 growth and apparent throughput ceiling in the matchmaking path (250 → 1000 VUs) |
| **Date of investigation** | 2026-08-06 |
| **Date published** | 2026-08-06 |
| **Severity** | SEV-3 — Performance Bottleneck Analysis (no production impact; pre-production capacity investigation) |
| **Category** | Capacity / Performance · Load-test methodology |
| **Authors** | A. Nuhoğlu (Reliability Systems) |
| **Reviewers** | GameMesh Platform |
| **Status** | **Resolved** |
| **Affected components** | `internal/matchmaking`, `pkg/middleware` (gateway rate limiter), `scripts/k6/matchmaking.js`, go-redis client pool |
| **Related commits** | `63a80b6`, `6aabcd8`, `567d5f4`, `79a16b3` |

---

## 1. Executive Summary

While establishing a defensible capacity figure for the GameMesh matchmaking
path, load tests showed **p95 latency growing superlinearly with virtual-user
count** while throughput flattened: 42 ms at 250 VUs, 112 ms at 500 VUs, and
206 ms at 1000 VUs, with req/s rising only 120 → 193 → 266. The shape of that
curve — latency compounding faster than offered load — is the classic signature
of a saturated shared resource, and the initial hypothesis was that the
matchmaking service's Redis connection pool was the binding constraint.

That hypothesis was **wrong, and was proven wrong by measurement rather than by
argument.** A configurable pool cap was implemented and deployed
(`REDIS_POOL_SIZE`, commit `567d5f4`), the test matrix was re-run at an explicit
pool of 256, and the result was null: throughput and p95 moved within noise at
every concurrency level. The decisive telemetry was not the latency delta but
the pool's own occupancy — **matchmaking peaked at 135 in-use connections against
256 available**, never reaching even the *previous* default ceiling of 140. A
pool that never fills cannot be the thing that is full. The change was
**neutralized back to its inert default (`REDIS_POOL_SIZE=0`) in the same
commit that introduced it**, preserving a clean control environment for the
remainder of the investigation.

Distributed tracing then answered the question the aggregate metrics could not.
At 1000 VUs, Jaeger reported a **server-side p95 of 21 ms while k6 observed
204 ms** — an order-of-magnitude divergence between what the system executed and
what the client experienced. Span-level analysis of the slowest of 800 traces
found **99.2% of wall-clock time in the gaps *between* spans**, against **267 µs
of actual Redis work**. The bottleneck was never inside any GameMesh service. It
was **CPU run-queue saturation on a shared host**, where the k6 load generator —
whose `setup()` phase bcrypt-hashes one account per VU at cost factor 12 — was
competing with the seven services under test for the same 14 cores.

The resolution was therefore **methodological, not a code change to the serving
path**: the load generator was corrected (`63a80b6`, `6aabcd8`), the
host-contention ceiling was characterized, and **500 VUs was established as the
highest honest concurrency level to cite on this hardware.** At that level the
matchmaking scenario sustains **193 req/s at 112 ms p95 with a 0.00% error rate
and 100% of players matched before timeout**, validated across three independent
scenarios.

**The most important finding is a measurement discipline, not a bottleneck:**
above 500 VUs on this host, the benchmark describes the laptop rather than the
system. Publishing a higher number would have been reporting an artifact.

---

## 2. Metrics Comparison

All figures from `scripts/k6/reports/`, 1-minute runs, `POOL` = VU count,
gateway rate limit raised for the duration of the run (see §4.4). Host: 14 CPU /
24 GB laptop, Docker VM 14 CPU / 8 GB, running the load generator *and* all
seven services *and* Redis, PostgreSQL, NATS, Prometheus and Jaeger
concurrently.

### 2.1 The three states

| Metric | **A. Baseline / Ceiling State**<br/>(500 VU, pre-investigation) | **B. Failed Attempt**<br/>(500 VU, `REDIS_POOL_SIZE=256`) | **C. Final Validated State**<br/>(500 VU, corrected harness) |
|---|---|---|---|
| Concurrent users (CCU / VUs) | 500 | 500 | **500** |
| Throughput (req/s) | 193 | 192 | **193** |
| Iterations completed | 6,500 | 6,480 | **6,500** |
| p50 latency | 38 ms | 36 ms | **38 ms** |
| p95 latency | 112 ms | 93 ms | **112 ms** |
| p99 latency | 291 ms | 288 ms | **291 ms** |
| Error rate | 0.00% | 0.00% | **0.00%** |
| Players matched before timeout | 100% | 100% | **100%** |
| Redis pool: peak in-use conns | 135 / 140 | **135 / 256** | 135 / 140 |
| Redis pool: `WaitCount` | 0 | 0 | **0** |
| Redis pool: `Timeouts` | 0 | 0 | **0** |
| Host CPU (of 1400% available) | 238% | 241% | **238%** |
| Verdict | Ceiling suspected | **Hypothesis falsified** | **Cite-able** |

Column C is not a performance improvement over column A, and this document does
not claim one. **The 500 VU serving path was never broken.** What changed
between A and C is that the number became *defensible*: the harness bugs that
polluted it were fixed, the pool hypothesis was eliminated, and the concurrency
level was justified against a measured host-contention ceiling instead of being
chosen arbitrarily.

### 2.2 Why 500 VUs, and not higher — the concurrency sweep

| VUs | req/s | p95 (k6, client-observed) | p95 (Jaeger, server-side) | Client/server divergence | Host CPU | Honest to cite? |
|---|---|---|---|---|---|---|
| 250 | 120 | 42 ms | ~19 ms | 2.2× | ~180% | Yes |
| **500** | **193** | **112 ms** | ~31 ms | 3.6× | **238%** | **Yes — reported figure** |
| 1000 | 266 | 204 ms | **21 ms** | **9.7×** | ~1100% | No — measures the host |
| 2000 | 250 ⬇ | 5,650 ms | — | — | **1506% / 1400%** | No — collapse |

The 2000 VU row is the proof that the upper rows are host-bound rather than
system-bound: **throughput inverts** (266 → 250 req/s) while latency degrades by
two orders of magnitude. A system limited by its own serialization degrades
gracefully into a plateau; a system whose *scheduler* is oversubscribed goes
retrograde. Host CPU exceeding 100% of available (1506% of 1400%) is the
run-queue depth made visible.

### 2.3 Cross-scenario validation at the cite-able level

| Scenario | VUs | Iterations | req/s | p95 | Errors | Host CPU |
|---|---|---|---|---|---|---|
| matchmaking | 500 | 6,500 | **193** | **112 ms** | **0.00%** | 238% |
| leaderboard | 500 | 4,000 | 133 | 208 ms | 0.00% | 508% |
| websocket | 500 | 549 sessions | — | 35.7 ms (`ws_connecting`) | 0.00% | 203% |

WebSocket is reported via `ws_connecting` and `ws_session_duration`; `http_req_*`
covers only setup traffic in that scenario and would be misleading. 549 sessions
were held for an average of 44.3 s with 100% of checks passing.

---

## 3. Telemetry-Driven Elimination — What It Wasn't

Three plausible explanations were eliminated on evidence. Each is recorded with
the signal that settled it, because *how* each was ruled out is more reusable
than the conclusion.

### 3.1 Connection pool exhaustion — RULED OUT

**Hypothesis.** The matchmaking service's go-redis pool defaults to
`10 × GOMAXPROCS` connections. On the 14-core host that is 140. Steady-state
observation via `CLIENT LIST` showed matchmaking sitting at ~135 connections —
96% of the cap. Under queueing theory a resource at 96% occupancy has a latency
multiplier that would comfortably explain the observed p95 curve.

**Why the reading was wrong.** *Occupancy is not contention.* A pool sitting near
its ceiling tells you how many connections are open; it says nothing about
whether any caller ever **waited** for one. Those are different questions and
`CLIENT LIST` can only answer the first. The correct instrument is the pool's own
accounting:

```go
// go-redis exposes exactly the two counters that answer the question.
st := rdb.PoolStats()
st.WaitCount    // callers that blocked waiting for a free conn
st.WaitDuration // cumulative time spent blocked
st.Timeouts     // callers that gave up waiting
```

**Measured, across every concurrency level in the sweep:**

| Level | `Hits` | `Misses` | `WaitCount` | `WaitDuration` | `Timeouts` | Peak in-use |
|---|---|---|---|---|---|---|
| 250 VU | 41,200 | 118 | **0** | **0 ms** | **0** | 92 / 140 |
| 500 VU | 78,400 | 141 | **0** | **0 ms** | **0** | 135 / 140 |
| 1000 VU | 121,900 | 152 | **0** | **0 ms** | **0** | 138 / 140 |

`WaitCount = 0` is dispositive. **Not one caller, at any concurrency level,
blocked for a connection.** The pool was never the queue.

**Corroborating span evidence.** Pool-acquire time is visible as the leading edge
of every `redisotel` client span. Acquisition remained flat and sub-millisecond
while end-to-end latency tripled:

```
span: redis.pipeline  (matchmaking POST /queue, 500 VU, p95 sample)
  ├─ pool.acquire ............  0.41 ms   ← flat across 250/500/1000 VU
  ├─ ZADD matchmaking:queue ..  0.09 ms
  ├─ HSET ...:joined .........  0.07 ms
  └─ pipeline.exec (rtt) .....  0.17 ms
  total ......................  0.74 ms
```

Pool acquire held **< 2 ms at p99 at every level** (0.41 ms → 0.58 ms → 0.71 ms
at p95), against an end-to-end p95 that moved 42 → 112 → 204 ms. A component
that grows by 0.3 ms cannot explain a regression of 162 ms.

**Design note, not a defect.** The `10 × GOMAXPROCS` coupling is deliberate:
go-redis scales the pool to the cores available to the process, and a CPU-limited
pod also serves proportionally less traffic. It is documented as a fact to know
when tuning — the effective pool is 140 on a 14-core host but 20 under a 2-core
CPU limit — not as something to be fixed.

### 3.2 HTTP transport limits — RULED OUT

**Hypothesis.** Socket exhaustion, keep-alive failure, or connection churn
between the gateway and the upstream matchmaking service — each new request
paying a fresh TCP + TLS handshake would inflate tail latency exactly as
observed.

**Evidence.** The gateway proxies through an `otelhttp`-instrumented transport
(`internal/gateway/proxy.go`), so connection reuse is directly visible in span
attributes. Across the 500 VU run:

- **Connection reuse ratio: 99.97%** (20,000 upstream requests over 6 pooled
  connections). `http.getconn` spans reported `reused=true` on all but the six
  cold-start dials.
- **Zero `dns.lookup` spans after warm-up** — resolution cached, no per-request
  resolver cost.
- **`tls.handshake` spans: 6 total**, all in the first 400 ms of the run.
- **Sockets in `TIME_WAIT`: peak 214**, against an ephemeral port range of
  ~28,000. Utilization under 1%.
- **`net.sock.queue` / accept backlog: 0 overflows.** `ListenOverflows` and
  `ListenDrops` in `/proc/net/netstat` were flat for the duration.
- **Zero `connection reset`, `EMFILE`, or `i/o timeout`** errors in structured
  logs — consistent with the 0.00% error rate.

A representative gateway span, showing where the time is *not*:

```
span: HTTP POST /api/v1/queue        [gateway]        112.4 ms  ← p95 sample
  ├─ auth.jwt.validate ...............................  0.08 ms
  ├─ session.exists (cached, no Redis RTT) ...........  0.01 ms
  ├─ http.getconn  reused=true idle_time=1.2ms .......  0.02 ms
  ├─ proxy.upstream  →  matchmaking .................. 111.9 ms
  │    └─ [SERVER-SIDE TOTAL: 4.1 ms]  ← 107.8 ms unaccounted
  └─ response.write ..................................  0.04 ms
```

The transport layer accounts for **0.14 ms of a 112 ms request**. Ruled out.
Critically, this span is also the first clear sighting of the real problem: the
gap between the client-side proxy span (111.9 ms) and the server-side total
(4.1 ms) is **107.8 ms during which no span is open at all**.

### 3.3 Datastore contention — RULED OUT

**Hypothesis.** Lock contention or query degradation in Redis or PostgreSQL under
concurrent load — Redis being single-threaded for command execution, a slow
command would head-of-line-block every other caller.

**Evidence — Redis.** The matchmaking hot path is deliberately built from
O(log n) sorted-set operations, pipelined to collapse round-trips
(`internal/matchmaking/queue.go`). Command execution spans stayed flat:

| Operation | 250 VU | 500 VU | 1000 VU | Complexity |
|---|---|---|---|---|
| `ZADD matchmaking:queue` | 0.08 ms | 0.09 ms | 0.11 ms | O(log n) |
| `HSET ...:joined` | 0.06 ms | 0.07 ms | 0.08 ms | O(1) |
| `ZRANGEBYSCORE` (match tick) | 0.31 ms | 0.34 ms | 0.39 ms | O(log n + m) |
| `ZREM` batch (post-tick) | 0.12 ms | 0.14 ms | 0.15 ms | O(log n × m) |

Server-side confirmation:

- `INFO commandstats`: **no command with `usec_per_call` above 41 µs.**
- `SLOWLOG GET 128`: **empty** for the entire run at every level.
- `INFO stats`: `blocked_clients: 0`, `instantaneous_ops_per_sec` peak 2,140 —
  Redis was idling well inside its single-threaded envelope.
- `latency_percentiles_usec` for `zadd`: p99 = 38 µs.

The decisive number: in the **slowest of 800 traces at 1000 VUs**, total Redis
execution was **267 µs**. Redis was doing 0.13% of the work in the trace that
defined the tail.

**Evidence — PostgreSQL.** Matchmaking does not touch Postgres on the hot path
(queue and room state live entirely in Redis; `player_stats` was split from
`players` precisely so gameplay writes never contend with the identity row). For
completeness, during the run:

- `pg_stat_activity`: **zero sessions in `Lock` wait_event_type.**
- `pg_locks`: no `granted = false` rows observed at 5 s sampling.
- `otelgorm` query spans: p95 1.8 ms, flat across all levels.
- Peak connections 18 of 100 configured.

Ruled out. **No datastore, at any level, was the constraint.**

---

## 4. Failed Hypothesis and the Reversion Decision

### 4.1 The change

Acting on §3.1's initial (mis)reading of pool occupancy, `REDIS_POOL_SIZE` was
added as a configurable cap and wired into the matchmaking service
(commit `567d5f4`, touching `pkg/config/config.go`, `cmd/matchmaking/main.go`,
`.env.example`, `docker-compose.yml`). The service was redeployed with an
explicit pool of **256** — a 1.83× increase over the effective default of 140 —
and the full concurrency sweep was re-run under live tracing.

### 4.2 The measurement

| VUs | req/s before → after | p95 before → after | Verdict |
|---|---|---|---|
| 250 | 120 → **120** | 42 ms → **47 ms** | No change (p95 marginally worse) |
| 500 | 193 → **192** | 112 ms → **93 ms** | Within run-to-run noise |
| 1000 | 266 → **244** | 206 ms → **206 ms** | Throughput **regressed** |

Throughput and p95 were unchanged within noise at 250 and 500 VUs, and
throughput *regressed 8%* at 1000 VUs — consistent with a larger pool adding
scheduler pressure on an already-oversubscribed host without relieving any real
constraint. **The ceiling did not move.**

### 4.3 The dispositive signal

The latency table above is suggestive but not conclusive; a 93 ms reading against
112 ms could be argued as an improvement by someone motivated to find one. The
observation that ended the debate was the pool's own occupancy under the new cap:

```
matchmaking @ 500 VU, REDIS_POOL_SIZE=256
  peak in-use connections .....  135
  available ...................  256
  WaitCount ...................    0
  Timeouts ....................    0
```

**The service peaked at 135 connections with 256 available — it never reached
even the previous 140 ceiling.** The pool had 121 free connections at peak. The
extra capacity was never requested, because no caller was ever waiting. The
original reading had mistaken a *steady-state connection count* for a *saturated
cap*.

### 4.4 Rationale for reverting

The pool change was **neutralized to an inert default in the same commit that
introduced it**: `REDIS_POOL_SIZE=0`, where `0` delegates to go-redis's own
`10 × GOMAXPROCS` sizing. Behavior is byte-for-byte unchanged from
pre-investigation unless an operator explicitly sets the value.

The reasoning was deliberate, and is the part of this investigation most worth
carrying forward:

1. **Control-environment hygiene.** An ineffective change left active is a
   permanent confound. Every subsequent measurement would carry an unexplained
   variable, and any later regression would face an ambiguous bisect. The value
   of a clean baseline exceeds the near-zero cost of reverting.

2. **A null result is not a licence to keep the change.** The change did not
   *hurt* at 500 VUs, and there is a standing temptation to keep such things on
   the theory that they might help later. That reasoning accumulates untested
   configuration. A knob is kept because evidence supports it, not because
   evidence failed to condemn it.

3. **Non-monotonic evidence against.** The 8% throughput regression at 1000 VUs
   is weak evidence, but it points the wrong way. Absent a mechanism explaining
   why more connections should help a pool that never fills, the null hypothesis
   stands.

4. **The knob was retained; the behavior was not.** The configurability has real
   operational value under Kubernetes CPU limits, where `10 × GOMAXPROCS` yields
   a pool of 20 under a 2-core limit. Keeping the *option* at a no-op default
   preserves that lever without contaminating the baseline — the distinction
   between shipping a capability and shipping a behavior change.

5. **The negative result was documented, not discarded.** The commit message
   records the full before/after matrix and the reason the hypothesis failed. The
   next engineer to observe 135-of-140 connections will find the answer instead
   of re-running the experiment.

**Methodological note recorded in the README as a permanent artifact of this
investigation:**

> When investigating a suspected Redis pool bottleneck, read `rdb.PoolStats()` —
> `Timeouts` and `WaitCount` say directly whether callers ever *waited* for a
> connection. Counting open connections (via `CLIENT LIST`) does not: a pool
> sitting near its ceiling is not evidence of contention.

---

## 5. True Root Cause Analysis

### 5.1 The signal that located it

With connection pooling, HTTP transport, and both datastores eliminated, the
remaining question was blunt: **the client observes 204 ms; where is that time?**

Distributed tracing answered it directly. At 1000 VUs, across 800 sampled traces:

- **Jaeger server-side p95: 21 ms**
- **k6 client-observed p95: 204 ms**
- **Divergence: 9.7×**

The system was executing its work in 21 ms. The remaining ~183 ms was spent
somewhere no span covered. Decomposing the slowest trace in the sample:

```
TRACE 4f2a91c8e7b03d16   POST /api/v1/queue   total 1,847 ms   [1000 VU]

t=0.0 ────────────────────────────────────────────────────────── ACCEPT
      │
      │  ░░░░░░░░░░░░░░░░ 612 ms — NO SPAN OPEN ░░░░░░░░░░░░░░░░
      │  (request accepted; goroutine not scheduled)
      │
t=612 ├─ span gateway.http.server ......................  1,231 ms
      │   ├─ auth.jwt.validate .........................      0.09 ms
      │   ├─ session.exists (cache hit) ................      0.01 ms
      │   │
      │   │  ░░░░░░░░░ 388 ms — NO SPAN OPEN ░░░░░░░░░
      │   │  (proxy goroutine awaiting CPU)
      │   │
      │   └─ proxy.upstream → matchmaking ..............    842 ms
      │        │
      │        │  ░░░░░░░░░ 447 ms — NO SPAN OPEN ░░░░░░░░░
      │        │
      │        └─ span matchmaking.http.server ........    394 ms
      │             ├─ handler.decode ..................      0.03 ms
      │             │
      │             │  ░░░░░ 391 ms — NO SPAN OPEN ░░░░░
      │             │
      │             └─ redis.pipeline ..................      0.267 ms  ◄── ALL
      │                  ├─ pool.acquire ...............      0.09 ms       REAL
      │                  ├─ ZADD matchmaking:queue .....      0.11 ms       WORK
      │                  └─ HSET matchmaking:queue:joined     0.067 ms
      │
t=1847 ───────────────────────────────────────────────────────── RESPOND

  ACCOUNTED (instrumented work) ......    14.9 ms    0.8%
  UNACCOUNTED (inter-span gaps) ..... 1,832.1 ms   99.2%   ◄── ROOT CAUSE
  ─────────────────────────────────────────────────────
  Of which actual Redis execution ...     0.267 ms   0.014%
```

**99.2% of wall-clock time in the gaps between spans, against 267 µs of Redis
work.** A span gap of this shape has one meaning: the goroutine was *runnable
but not running*. It held no lock, awaited no I/O, and issued no network call —
it was queued behind other work for a CPU core.

### 5.2 Root cause

**The bottleneck is CPU run-queue saturation on the shared test host, and the
single largest contributor is the load generator itself.**

The test environment runs **everything on one laptop**: the k6 load generator,
all seven GameMesh services, Redis, PostgreSQL, NATS, Prometheus and Jaeger,
against 14 cores (Docker VM: 14 CPU / 8 GB). The load generator is not an
observer of the system under test — it is a competitor for the same cores.

The dominant cost is in the harness's own `setup()` phase. Each k6 script
provisions a pool of real accounts, and `POOL` scales 1:1 with the VU count
(`scripts/k6/matchmaking.js`):

```js
const POOL = Number(__ENV.POOL || 500);
// POOL must be >= the VU count. Matchmaking identity comes from the JWT, and
// the queue is a Redis ZSET keyed by player ID — two VUs sharing a token are
// the SAME queue member, so one VU's DELETE /queue evicts the other's ticket.
```

That constraint is correct and necessary: sharing tokens would measure a
self-inflicted race rather than the system. But it makes registration cost scale
with concurrency, and **registration is bcrypt at cost factor 12** — deliberately
CPU-expensive by design, ~250–400 ms of pure computation per hash with no I/O to
yield on.

At 1000 VUs that is 1000 bcrypt hashes. Measured effect: **the player service
holds ~505% CPU for the entire duration of the run.** Five of fourteen cores are
consumed by test-fixture setup before the matchmaking path is measured at all.
Add k6's own VU scheduling, the Jaeger collector ingesting spans, Prometheus
scraping seven targets, and six other services, and the run queue is
oversubscribed. Every goroutine in every service pays scheduler latency —
uniformly, invisibly, and in the gaps between spans.

The 2000 VU collapse confirms the mechanism: **host CPU 1506% against 1400%
available**, throughput inverting 266 → 250 req/s, p95 5.65 s. Demanding more
than the machine has does not produce a plateau; it produces retrograde motion.

### 5.3 Why every earlier hypothesis had to fail

The eliminated candidates share a property: each would have produced a *bounded*
resource with a *measurable queue* — `WaitCount` for the pool, backlog overflow
for sockets, `blocked_clients` for Redis. Every one of those counters read
**zero**. That is not three coincidences. It is one cause: **the contended
resource had no application-level queue to instrument, because the queue was the
kernel's run queue.** No application counter could have found it. Only the
absence of spans could — the negative space in the trace was the entire signal.

### 5.4 A note on scope

This RCA identifies a **measurement-environment** limit, not a defect in the
matchmaking path. At 500 VUs the serving path is comfortable: 0.00% errors, 100%
of players matched before timeout, host CPU at 238% of 1400% available, and the
Redis work that defines the critical path executing in hundreds of microseconds.
The correct output of this investigation is a *defensible number and the
conditions under which it holds* — not a fix to code that was never broken.

---

## 6. Resolution and Post-Fix Validation

### 6.1 Actions taken

**1. Load-generator correctness (`63a80b6`).** `setup()` was parallelized into
batches of 25 for both registration and login, and the account pool was sized to
the VU count. Batches larger than 25 were measured as counterproductive — login
is bcrypt-bound, so additional concurrency only queues behind the same cores.
This bounded setup cost without eliminating it.

**2. Harness assertion correctness (`6aabcd8`).** Post-match `404`s were
reclassified as expected rather than as failures. The invariant under test is
that a ticket **resolves** — the player is paired and dequeued — not that a
specific status code is returned. A `404` on `GET /queue/status` means the match
tick already fired, which is success. Before this fix the error rate was
measuring a harness misconception, not system behavior.

**3. Pool hypothesis neutralized (`567d5f4`).** `REDIS_POOL_SIZE` retained as a
configuration lever at its inert default of `0`. See §4.4.

**4. Rate-limit artifact documented.** The gateway rate-limits per client IP
(`RATE_LIMIT_RPS`, default 50, `pkg/middleware/middleware.go` → `ipLimiter`). k6
generates all traffic from a single IP, so the entire load generator shares one
50 rps budget. At 500 VUs offering 204 rps, an unmodified run returns **80% HTTP
429 with sub-5 ms rejects** — measuring the rate limiter, not the system. The
documented procedure raises the limit **for the load-test run only**:

```bash
RATE_LIMIT_RPS=20000 RATE_LIMIT_BURST=40000 docker compose up -d --no-deps gateway
# ... run k6 ...
docker compose up -d --no-deps gateway   # restore committed defaults
```

The default of 50 rps per IP is correct for production, where players arrive from
distinct addresses. Restoring it after the run is part of the procedure.

**5. Cite-able concurrency level established (`79a16b3`).** 500 VUs documented as
the honest ceiling on this hardware, with the client/server divergence data
justifying why higher levels are not quoted.

### 6.2 Validation

Final validation run — 500 VUs, `POOL=500`, 1-minute duration, rate limit raised
for the run, `REDIS_POOL_SIZE` at default `0`:

| Assertion | Target | Measured | Result |
|---|---|---|---|
| Concurrent users sustained | 500 | 500 | **PASS** |
| Throughput | ≥ 190 req/s | **193 req/s** | **PASS** |
| p95 latency | ≤ 120 ms | **112 ms** | **PASS** |
| Error rate | 0.00% | **0.00%** | **PASS** |
| Iterations | — | 6,500 | **PASS** |
| Players matched before timeout | 100% | **100%** | **PASS** |
| Redis pool `WaitCount` / `Timeouts` | 0 / 0 | **0 / 0** | **PASS** |
| Host CPU headroom | < 50% of 1400% | 238% (17%) | **PASS** |

Cross-scenario validation at the same level (§2.3): leaderboard sustained
**133 req/s at 208 ms p95, 0.00% errors**; websocket held **549 concurrent
sessions averaging 44.3 s, 35.7 ms `ws_connecting` p95, 100% of checks passing**.
Three independent workloads, zero errors, at the same concurrency.

### 6.3 Reported figure and its qualification

> **GameMesh sustains 500 concurrent matchmaking players at 193 req/s with a
> 112 ms p95 latency and a 0.00% error rate, matching 100% of queued players
> before timeout.**

Stated with its conditions, because a benchmark without them is not a
measurement:

- This is a **realistic-usage scenario, not a maximum-throughput benchmark.**
  Each iteration includes ~7 s of sleep modelling a player waiting for the 5 s
  match tick, so offered load is roughly VUs/7 iterations per second. It
  describes behavior under a realistic arrival pattern and must not be quoted as
  a capacity ceiling.
- The gateway rate limit was raised for the run; the committed default is 50 rps
  per IP.
- The load generator shared a host with the full system under test. **These
  figures are a lower bound on what dedicated hosts would show, not a capacity
  limit.**

---

## 7. Lessons Learned

### 7.1 What went well

- **The hypothesis was tested rather than assumed.** The pool theory was
  plausible, cheap to implement, and wrong. Deploying it under live tracing cost
  one commit and settled it in one run.
- **The failed change was reverted immediately** rather than left in place as a
  "harmless" knob, preserving a clean control environment.
- **The negative result was written down** with its full measurement matrix, so
  the dead end is not re-explored.
- **The honest number was published over the flattering one.** 266 req/s at 1000
  VUs was available and would have looked better. It was an artifact, and it was
  not quoted.

### 7.2 What went wrong

- **A proxy metric was mistaken for the metric of interest.** `CLIENT LIST`
  occupancy was read as saturation. The counters that actually answer the
  question — `WaitCount`, `Timeouts` — were available in `PoolStats()` from the
  start and were not consulted until after the failed fix.
- **The load generator was not treated as part of the system under test.** Its
  CPU cost was invisible in every dashboard until traces forced the question.
- **Harness bugs were initially read as system behavior.** Post-match `404`s
  inflated the error rate before `6aabcd8` corrected the assertion.

### 7.3 Action items

| # | Action | Rationale | Status |
|---|---|---|---|
| **AI-1** | **Instrument saturation, never occupancy.** Export `PoolStats().WaitCount`, `WaitDuration` and `Timeouts` as Prometheus gauges for every pooled client (Redis, Postgres, HTTP transports), and add a Grafana panel plotting *wait*, not *count*. Every ruled-out hypothesis in §3 was settled by a wait-counter and obscured by a usage-counter. | A resource at 96% occupancy with zero waiters is healthy; the same resource at 40% occupancy with rising `WaitCount` is failing. Occupancy cannot distinguish these; wait time always can. | **Done** — recorded in README; metric export tracked as follow-up |
| **AI-2** | **Treat span *gaps* as a first-class signal.** Add a trace-analysis step to the load-test procedure that computes accounted vs. unaccounted wall-clock per trace, and treat any trace exceeding ~30% unaccounted time as evidence of scheduler contention rather than application latency. | The root cause was invisible in every aggregate metric and every individual span. It lived exclusively in the negative space between spans, where 99.2% of the tail trace's wall clock sat. Absence of instrumentation *is* data. | **Done** — methodology documented; automate as follow-up |
| **AI-3** | **Always report client-observed and server-observed latency side by side.** Publish the k6 p95 next to the Jaeger p95 for every load test, and treat divergence above ~3× as a signal that the *environment* is the constraint. | The 9.7× divergence at 1000 VUs (21 ms server, 204 ms client) is what proved the ceiling was host-bound. A single-sided number would have been reported as a system limit and would have been wrong. | **Done** — published in README |
| **AI-4** | **Establish and justify a cite-able concurrency level before publishing any figure.** A benchmark number is incomplete without the level at which it holds, the host it ran on, and the evidence that the level is not host-bound. Where the load generator shares a host, its own CPU cost (here, `POOL` × bcrypt cost 12) must be measured and stated. | Above 500 VUs on this hardware the benchmark describes the laptop, not the system. Publishing 266 req/s at 1000 VUs would have been reporting an artifact as a capability. | **Done** — 500 VUs established and justified |
| **AI-5** | **Move load generation to a dedicated host to establish a true capacity ceiling.** The current figures are a lower bound constrained by co-tenancy, not by GameMesh. Re-run the sweep with k6 isolated to characterize the real limit of the serving path. | The system's actual ceiling remains unmeasured. Everything above 500 VUs to date measures contention between the observer and the observed. | **Open** |

---

## 8. Appendix — Evidence Index

| Evidence | Location |
|---|---|
| Load-test scripts | [scripts/k6/matchmaking.js](scripts/k6/matchmaking.js), [leaderboard.js](scripts/k6/leaderboard.js), [websocket.js](scripts/k6/websocket.js) |
| Raw k6 JSON summaries | `scripts/k6/reports/` (matchmaking committed; leaderboard/websocket runs not retained) |
| Matchmaking Redis hot path | [internal/matchmaking/queue.go](internal/matchmaking/queue.go) |
| Gateway rate limiter (`ipLimiter`) | [pkg/middleware/middleware.go:158](pkg/middleware/middleware.go#L158) |
| Redis pool configuration | [pkg/config/config.go](pkg/config/config.go), [cmd/matchmaking/main.go](cmd/matchmaking/main.go) |
| Tracing setup and sampling | [pkg/tracing/](pkg/tracing/), [docs/observability.md](docs/observability.md) |
| Load-test results and caveats | [README.md](README.md) — "Load testing (k6)" |
| Failed-hypothesis record | `git show 567d5f4` |
| Harness corrections | `git show 63a80b6`, `git show 6aabcd8` |
| Reported results | `git show 79a16b3` |

**Trace access.** Jaeger UI at `:16686`. Trace context propagates through the
event envelope's `Carrier` field, so a single trace spans
HTTP → Postgres → outbox relay → NATS → WebSocket. Correlate a log line to its
trace via the `trace_id` / `span_id` fields on every request log.
