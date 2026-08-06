// k6 load test: 1,000 concurrent WebSocket clients.
// Run:  WS_URL=ws://localhost:8084 BASE_URL=http://localhost:8080 k6 run scripts/k6/websocket.js
//       POOL=1000 k6 run --vus 1000 scripts/k6/websocket.js
//
// This test connects DIRECTLY to the websocket service (port 8084), not through
// the gateway, so it is not subject to the gateway's per-IP rate limit. Only the
// setup() registration/login calls go through the gateway.
//
// Report ws_connecting, ws_session_duration and checks for this scenario —
// http_req_* covers only the setup traffic and is not meaningful here.
import ws from "k6/ws";
import http from "k6/http";
import { check, sleep } from "k6";
import { Counter } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const WS_URL = __ENV.WS_URL || "ws://localhost:8084";
const POOL = Number(__ENV.POOL || 1000);
const eventsReceived = new Counter("ws_events_received");

export const options = {
  setupTimeout: "5m",
  scenarios: {
    websocket_clients: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: [
        { duration: "30s", target: 1000 }, // 1,000 concurrent WS clients
        { duration: "2m", target: 1000 },
        { duration: "30s", target: 0 },
      ],
    },
  },
  thresholds: {
    ws_connecting: ["p(95)<1000"],
    checks: ["rate>0.95"],
  },
};

// Register + log in POOL accounts in parallel batches of 25. Login is
// bcrypt-bound and CPU-bound in the auth service; larger batches do not help.
export function setup() {
  const stamp = Date.now() % 100000;
  const names = [];
  for (let i = 0; i < POOL; i++) names.push(`wsuser${i}${stamp}`);
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
  const url = `${WS_URL}/ws?token=${token}`;
  const room = `load-room-${__VU % 50}`;

  const res = ws.connect(url, {}, function (socket) {
    socket.on("open", function () {
      // Subscribe to a shared room so PlayerJoined events fan out.
      socket.send(JSON.stringify({ action: "join", room }));
    });

    socket.on("message", function () {
      eventsReceived.add(1);
    });

    // Hold the connection open for 30-60s like a real client.
    socket.setTimeout(function () {
      socket.send(JSON.stringify({ action: "leave", room }));
      socket.close();
    }, 30000 + Math.random() * 30000);
  });

  check(res, { "ws upgrade 101": (r) => r && r.status === 101 });
  sleep(1);
}
