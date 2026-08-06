// k6 load test: 5,000 concurrent matchmaking requests.
// Run:  BASE_URL=http://localhost:8080 k6 run scripts/k6/matchmaking.js
//       POOL=500 k6 run --vus 500 --duration 1m scripts/k6/matchmaking.js
//
// POOL must be >= the VU count. Matchmaking identity comes from the JWT, and the
// queue is a Redis ZSET keyed by player ID, so two VUs sharing a token are the
// SAME queue member — one VU's DELETE would evict the other's ticket.
import http from "k6/http";
import { check, sleep } from "k6";
import { Trend, Rate } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const POOL = Number(__ENV.POOL || 500);
const queueJoinTime = new Trend("queue_join_duration", true);
const matchedRate = new Rate("matched_before_timeout");

// GET /queue/status always answers 200 ({"queued":bool,"queue_size":n}) — it
// never 404s, so the real success signal is the `queued` field, not the code.
// DELETE /queue answers 404 ("player not in queue") once the match tick has
// already dequeued the player — that is the SUCCESS path, so 404 is expected.
const expectOkOrMissing = http.expectedStatuses(200, 404);

export const options = {
  setupTimeout: "5m",
  scenarios: {
    matchmaking_surge: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: [
        { duration: "30s", target: 1000 },
        { duration: "1m", target: 5000 }, // 5,000 concurrent players queueing
        { duration: "1m", target: 5000 },
        { duration: "30s", target: 0 },
      ],
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.02"],
    http_req_duration: ["p(95)<500"],
  },
};

// Register + log in POOL accounts. Both phases run in parallel batches of 25:
// login is bcrypt-bound (~185ms each) and CPU-bound in the auth service, so
// larger batches just queue behind the same cores without going faster.
export function setup() {
  const stamp = Date.now() % 100000;
  const names = [];
  for (let i = 0; i < POOL; i++) names.push(`mmuser${i}${stamp}`);
  const hdrs = { headers: { "Content-Type": "application/json" } };

  for (let i = 0; i < names.length; i += 25) {
    http.batch(
      names.slice(i, i + 25).map((u) => [
        "POST",
        `${BASE_URL}/api/v1/auth/register`,
        JSON.stringify({ username: u, email: `${u}@example.com`, password: "password123" }),
        hdrs,
      ])
    );
  }

  const tokens = [];
  for (let i = 0; i < names.length; i += 25) {
    const res = http.batch(
      names.slice(i, i + 25).map((u) => [
        "POST",
        `${BASE_URL}/api/v1/auth/login`,
        JSON.stringify({ identifier: u, password: "password123" }),
        hdrs,
      ])
    );
    res.forEach((r) => {
      if (r.status === 200) tokens.push(r.json("token"));
    });
  }

  if (tokens.length === 0) throw new Error("setup: no tokens obtained");
  console.log(`setup: ${tokens.length}/${POOL} tokens ready`);
  return { tokens };
}

export default function (data) {
  const token = data.tokens[__VU % data.tokens.length];
  const headers = {
    "Content-Type": "application/json",
    Authorization: `Bearer ${token}`,
  };

  // Join the queue with a realistic rank distribution around 1000.
  const rank = Math.max(0, Math.round(1000 + (Math.random() - 0.5) * 600));
  const join = http.post(`${BASE_URL}/api/v1/queue`, JSON.stringify({ rank }), {
    headers,
  });
  check(join, { "queued": (r) => r.status === 200 });
  queueJoinTime.add(join.timings.duration);

  // Poll status while waiting for the 5s match tick. The invariant under test is
  // that the ticket RESOLVES (the player gets matched and dequeued) — not that a
  // particular status code comes back.
  let matched = false;
  for (let i = 0; i < 3 && !matched; i++) {
    sleep(2);
    const status = http.get(`${BASE_URL}/api/v1/queue/status`, {
      headers,
      responseCallback: expectOkOrMissing,
    });
    check(status, {
      "status readable": (r) => r.status === 200 || r.status === 404,
    });
    // queued:false means the match tick already paired and dequeued this player.
    if (status.status === 404 || (status.status === 200 && status.json("queued") === false)) {
      matched = true;
    }
  }
  matchedRate.add(matched);

  // Only leave if still queued (covers the unmatched case). A 404 here is still
  // legitimate: the tick may have fired between the last poll and this call.
  if (!matched) {
    const left = http.del(`${BASE_URL}/api/v1/queue`, null, {
      headers,
      responseCallback: expectOkOrMissing,
    });
    check(left, {
      "queue entry resolved": (r) => r.status === 200 || r.status === 404,
    });
  }
  sleep(1);
}
