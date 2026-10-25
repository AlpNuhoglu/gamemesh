// k6 load for the public status page (phase 2, G7): enough traffic for its
// four panels to move, safe to run against production.
//
//   BASE_URL=https://api.gamemesh.app WS_URL=wss://api.gamemesh.app \
//     k6 run --summary-export=summary.json scripts/k6/status-demo.js
//
// What it writes, on purpose little: POOL accounts named k6g7<n><stamp>
// (alphanumeric, the only characters usernames allow), logged out again in
// teardown. They post no score and join no queue or room, so they never show
// on the public leaderboard; delete them afterwards (docs/deployment.md,
// "Status page"). Everything else is a read.
//
// The shape is a ramp, so the panels show a clear rise and fall: up over RAMP,
// steady for HOLD, down over RAMP.
//   reads:   GET /api/v1/leaderboard and /top/10, up to RATE requests/s. The
//            gateway limits each client address to 50/s (burst 100); stay
//            below it.
//   sockets: up to POOL WebSocket connections through the gateway (/ws), each
//            held open, which is what the active connections panel counts.
//
// Report the scenarios' own figures (http_req_duration{scenario:reads}):
// the plain http_req_duration also counts setup's register and login calls,
// which take about 200 ms each (bcrypt) and distorted earlier p95s
// (finding #91).
import http from "k6/http";
import ws from "k6/ws";
import { check, sleep } from "k6";
import { Rate } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const WS_URL = __ENV.WS_URL || "ws://localhost:8080";
const POOL = Number(__ENV.POOL || 20);
const RATE = Number(__ENV.RATE || 30);
const RAMP = __ENV.RAMP || "1m";
const HOLD = __ENV.HOLD || "3m";
const JSON_HDRS = { headers: { "Content-Type": "application/json" } };
// Recorded when a socket opens (or fails to): a check after ws.connect would
// only run once the connection closes, which a short run never reaches, and
// a threshold without samples passes.
const wsOpened = new Rate("ws_opened");

export const options = {
  setupTimeout: "2m",
  scenarios: {
    reads: {
      executor: "ramping-arrival-rate",
      exec: "reads",
      startRate: 0,
      timeUnit: "1s",
      preAllocatedVUs: 20,
      maxVUs: 60,
      stages: [
        { duration: RAMP, target: RATE },
        { duration: HOLD, target: RATE },
        { duration: RAMP, target: 0 },
      ],
    },
    sockets: {
      executor: "ramping-vus",
      exec: "sockets",
      startVUs: 0,
      stages: [
        { duration: RAMP, target: POOL },
        { duration: HOLD, target: POOL },
        { duration: RAMP, target: 0 },
      ],
      gracefulRampDown: "5s",
    },
  },
  // Thresholds on the scenarios' own sub-metrics also make k6 print them
  // separately from setup's traffic.
  thresholds: {
    "http_req_duration{scenario:reads}": ["p(95)<1000"],
    "http_req_failed{scenario:reads}": ["rate<0.01"],
    "ws_connecting{scenario:sockets}": ["p(95)<2000"],
    ws_opened: ["rate>0.95"],
  },
};

export function setup() {
  const stamp = Date.now() % 100000;
  const names = [];
  for (let i = 0; i < POOL; i++) names.push(`k6g7${i}${stamp}`);
  http.batch(
    names.map((u) => [
      "POST",
      `${BASE_URL}/api/v1/auth/register`,
      JSON.stringify({ username: u, email: `${u}@example.com`, password: "k6g7-password" }),
      JSON_HDRS,
    ])
  );
  const tokens = http
    .batch(
      names.map((u) => [
        "POST",
        `${BASE_URL}/api/v1/auth/login`,
        JSON.stringify({ identifier: u, password: "k6g7-password" }),
        JSON_HDRS,
      ])
    )
    .filter((r) => r.status === 200)
    .map((r) => r.json("token"));
  if (tokens.length === 0) throw new Error("setup: no tokens obtained");
  console.log(`setup: ${tokens.length}/${POOL} accounts (k6g7*${stamp}) logged in`);
  return { tokens };
}

export function reads() {
  const path = __ITER % 2 === 0 ? "/api/v1/leaderboard" : "/api/v1/leaderboard/top/10";
  const res = http.get(`${BASE_URL}${path}`);
  check(res, { "read 200": (r) => r.status === 200 });
}

export function sockets(data) {
  const token = data.tokens[(__VU - 1) % data.tokens.length];
  let opened = false;
  const res = ws.connect(`${WS_URL}/ws?token=${token}`, {}, function (socket) {
    socket.on("open", () => {
      opened = true;
      wsOpened.add(true);
    });
    // Held for most of the steady phase; the scenario's ramp-down ends it.
    socket.setTimeout(() => socket.close(), 90000);
  });
  if (!opened) wsOpened.add(false);
  check(res, { "ws upgrade 101": (r) => r && r.status === 101 });
  sleep(1);
}

// The sessions end now rather than when the tokens expire.
export function teardown(data) {
  http.batch(
    data.tokens.map((t) => [
      "POST",
      `${BASE_URL}/api/v1/auth/logout`,
      null,
      { headers: { Authorization: `Bearer ${t}` } },
    ])
  );
}
