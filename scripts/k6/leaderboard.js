// k6 load test: 10,000 leaderboard score updates + read mix.
// Run:  BASE_URL=http://localhost:8080 k6 run scripts/k6/leaderboard.js
// Report: k6 run --summary-export=scripts/k6/reports/leaderboard.json ...
import http from "k6/http";
import { check, sleep } from "k6";
import { Counter } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const scoreUpdates = new Counter("score_updates");

export const options = {
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

// Each VU registers + logs in once, then hammers POST /score.
export function setup() {
  const tokens = [];
  for (let i = 0; i < 100; i++) {
    const username = `lbuser${i}${Date.now() % 100000}`;
    const reg = http.post(
      `${BASE_URL}/api/v1/auth/register`,
      JSON.stringify({
        username: username,
        email: `${username}@example.com`,
        password: "password123",
      }),
      { headers: { "Content-Type": "application/json" } }
    );
    check(reg, { "registered": (r) => r.status === 201 });

    const login = http.post(
      `${BASE_URL}/api/v1/auth/login`,
      JSON.stringify({ identifier: username, password: "password123" }),
      { headers: { "Content-Type": "application/json" } }
    );
    check(login, { "logged in": (r) => r.status === 200 });
    tokens.push(login.json("token"));
  }
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
