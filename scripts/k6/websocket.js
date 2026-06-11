// k6 load test: 1,000 concurrent WebSocket clients.
// Run:  WS_URL=ws://localhost:8084 BASE_URL=http://localhost:8080 k6 run scripts/k6/websocket.js
import ws from "k6/ws";
import http from "k6/http";
import { check, sleep } from "k6";
import { Counter } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const WS_URL = __ENV.WS_URL || "ws://localhost:8084";
const eventsReceived = new Counter("ws_events_received");

export const options = {
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

export function setup() {
  const tokens = [];
  for (let i = 0; i < 100; i++) {
    const username = `wsuser${i}${Date.now() % 100000}`;
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
  const url = `${WS_URL}/ws?token=${token}`;

  const res = ws.connect(url, {}, function (socket) {
    socket.on("open", function () {
      // Subscribe to a shared room so PlayerJoined events fan out.
      socket.send(JSON.stringify({ action: "join", room: `load-room-${__VU % 50}` }));
    });

    socket.on("message", function () {
      eventsReceived.add(1);
    });

    // Hold the connection open for 30-60s like a real client.
    socket.setTimeout(function () {
      socket.send(JSON.stringify({ action: "leave", room: `load-room-${__VU % 50}` }));
      socket.close();
    }, 30000 + Math.random() * 30000);
  });

  check(res, { "ws upgrade 101": (r) => r && r.status === 101 });
  sleep(1);
}
