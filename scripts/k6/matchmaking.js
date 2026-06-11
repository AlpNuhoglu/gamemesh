// k6 load test: 5,000 concurrent matchmaking requests.
// Run:  BASE_URL=http://localhost:8080 k6 run scripts/k6/matchmaking.js
import http from "k6/http";
import { check, sleep } from "k6";
import { Trend } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const queueJoinTime = new Trend("queue_join_duration", true);

export const options = {
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

export function setup() {
  // A pool of shared accounts: matchmaking identity comes from the JWT.
  const tokens = [];
  for (let i = 0; i < 200; i++) {
    const username = `mmuser${i}${Date.now() % 100000}`;
    http.post(
      `${BASE_URL}/api/v1/auth/register`,
      JSON.stringify({
        username: username,
        email: `${username}@example.com`,
        password: "password123",
      }),
      { headers: { "Content-Type": "application/json" } }
    );
    const login = http.post(
      `${BASE_URL}/api/v1/auth/login`,
      JSON.stringify({ identifier: username, password: "password123" }),
      { headers: { "Content-Type": "application/json" } }
    );
    if (login.status === 200) tokens.push(login.json("token"));
  }
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
