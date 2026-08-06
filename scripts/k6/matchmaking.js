// k6 load test: 5,000 concurrent matchmaking requests.
// Run:  BASE_URL=http://localhost:8080 k6 run scripts/k6/matchmaking.js
//       POOL=500 k6 run --vus 500 --duration 1m scripts/k6/matchmaking.js
//
// POOL must be >= the VU count. Matchmaking identity comes from the JWT, and the
// queue is a Redis ZSET keyed by player ID, so two VUs sharing a token are the
// SAME queue member — one VU's DELETE would evict the other's ticket.
import http from "k6/http";
import { check, sleep } from "k6";
import { Trend } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const POOL = Number(__ENV.POOL || 500);
const queueJoinTime = new Trend("queue_join_duration", true);

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

  // Poll status a few times (players waiting for the 5s match tick).
  for (let i = 0; i < 3; i++) {
    sleep(2);
    const status = http.get(`${BASE_URL}/api/v1/queue/status`, { headers });
    check(status, { "status ok": (r) => r.status === 200 });
  }

  // Leave if still queued (covers the unmatched case).
  http.del(`${BASE_URL}/api/v1/queue`, null, { headers });
  sleep(1);
}
