// k6 load test: 10,000 leaderboard score updates + read mix.
// Run:  BASE_URL=http://localhost:8080 k6 run scripts/k6/leaderboard.js
//       POOL=100 k6 run --vus 100 scripts/k6/leaderboard.js
// Report: k6 run --summary-export=scripts/k6/reports/leaderboard.json ...
//
// POOL should be >= the VU count. Unlike matchmaking, a shared token here is
// only a fairness issue (several VUs incrementing one player's score), not a
// correctness one — but sizing the pool keeps the score distribution realistic.
//
// NOTE: the gateway rate-limits per client IP (RATE_LIMIT_RPS, default 50). All
// k6 traffic is one IP, so beyond ~50 rps this measures the rate limiter rather
// than the leaderboard. See README "Load testing".
import http from "k6/http";
import { check, sleep } from "k6";
import { Counter } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const POOL = Number(__ENV.POOL || 100);
const scoreUpdates = new Counter("score_updates");

export const options = {
  setupTimeout: "5m",
  scenarios: {
    score_updates: {
      executor: "shared-iterations",
      vus: 100,
      iterations: 10000, // 10,000 leaderboard updates total
      maxDuration: "5m",
    },
  },
  thresholds: {
    http_req_failed: ["rate<0.01"],
    http_req_duration: ["p(95)<300"],
  },
};

// Register + log in POOL accounts in parallel batches of 25. Login is
// bcrypt-bound (~185ms each) and CPU-bound in the auth service, so batches
// larger than ~25 just queue behind the same cores.
export function setup() {
  const stamp = Date.now() % 100000;
  const names = [];
  for (let i = 0; i < POOL; i++) names.push(`lbuser${i}${stamp}`);
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

  const res = http.post(
    `${BASE_URL}/api/v1/score`,
    JSON.stringify({ score: Math.floor(Math.random() * 100) + 1, increment: true }),
    { headers }
  );
  check(res, { "score accepted": (r) => r.status === 200 });
  if (res.status === 200) scoreUpdates.add(1);

  // Mix in reads: top-10 and paginated views.
  if (__ITER % 10 === 0) {
    const top = http.get(`${BASE_URL}/api/v1/leaderboard/top/10`);
    check(top, { "top-10 ok": (r) => r.status === 200 });
  }
  sleep(0.05);
}
