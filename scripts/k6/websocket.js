// k6 load test: 1,000 concurrent WebSocket clients, each going the way a real
// player does: connect, queue for a match, get MatchFound, join the room it
// names, stay 30-60 s, leave.
//
// Run:  WS_URL=ws://localhost:8084 BASE_URL=http://localhost:8080 k6 run scripts/k6/websocket.js
//       POOL=500 k6 run --vus 500 --duration 1m scripts/k6/websocket.js
//
// A room cannot be made up: the websocket service accepts a join only for a
// room matchmaking created, and only from one of its two players. The room's
// ID reaches a player in one way, as MatchFound on its open socket, so the
// session has to queue while connected. Every session therefore creates a
// match; rooms expire on their own (ROOM_TTL).
//
// The socket goes DIRECTLY to the websocket service (port 8084), not through
// the gateway. Setup's register and login calls and each session's one
// POST /api/v1/queue go through the gateway and its per-IP rate limit
// (RATE_LIMIT_RPS, default 50): the default ramp stays under it, but
// --vus 500 starts 500 sessions in one second and needs it raised (README,
// "Before citing numbers").
//
// POOL must be >= the VU count: a queue ticket and a room seat belong to a
// player, so two VUs sharing a token are one queue member.
//
// Report ws_connecting, ws_match_wait, ws_join_time, ws_session_duration and
// the rates below. http_req_* is setup's bcrypt-bound traffic plus one queue
// call per session and is not the figure of this scenario.
import ws from "k6/ws";
import http from "k6/http";
import { check, sleep } from "k6";
import { Counter, Rate, Trend } from "k6/metrics";

const BASE_URL = __ENV.BASE_URL || "http://localhost:8080";
const WS_URL = __ENV.WS_URL || "ws://localhost:8084";
const POOL = Number(__ENV.POOL || 1000);

// One rank for everyone, so any two queued players are inside the match
// window (MATCH_RANK_WINDOW) and pair on the next tick.
const RANK = 1000;
// The matcher ticks every 5 s (MATCH_INTERVAL_SECONDS). A session without a
// partner by then is the odd one out, at the end of a run.
const MATCH_TIMEOUT_MS = 30000;
// After the matcher has taken a ticket, how long its MatchFound may take.
const MATCH_GRACE_MS = 10000;
const JOIN_TIMEOUT_MS = 10000;

// POST /queue answered -> MatchFound arrived. Mostly the wait for the tick.
const matchWait = new Trend("ws_match_wait", true);
// join sent -> the room's PlayerJoined for this player came back.
const joinTime = new Trend("ws_join_time", true);
// Sessions that got a MatchFound.
const matched = new Rate("ws_matched");
// Matched sessions whose join was accepted.
const joined = new Rate("ws_joined");
// Joined sessions that saw the room's other member join or leave: a message
// fanned out inside a real room and reached this socket.
const roomFanout = new Rate("ws_room_fanout");
// A join the server refused. A room from MatchFound is this player's own, so
// any refusal is a failure.
const joinRejected = new Counter("ws_join_rejected");
const eventsReceived = new Counter("ws_events_received");

// DELETE /queue answers 404 once the matcher has taken the ticket.
const expectOkOrMissing = http.expectedStatuses(200, 404);
const LOAD = { phase: "load" };

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
      // A session lasts up to about 70 s and its partner depends on it: let
      // running sessions end instead of cutting them at the ramp-down.
      gracefulRampDown: "90s",
    },
  },
  thresholds: {
    ws_connecting: ["p(95)<1000"],
    checks: ["rate>0.95"],
    ws_matched: ["rate>0.95"],
    ws_joined: ["rate>0.99"],
    ws_room_fanout: ["rate>0.95"],
    ws_join_rejected: ["count<1"],
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

  // The player's ID comes along: PlayerJoined and PlayerLeft name a player,
  // and a session has to tell its own from its partner's.
  const players = [];
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
      if (r.status === 200) players.push({ token: r.json("token"), id: r.json("player.id") });
    });
  }

  if (players.length === 0) throw new Error("setup: no tokens obtained");
  if (!players[0].id) throw new Error("setup: login answered without player.id");
  console.log(`setup: ${players.length}/${POOL} players ready`);
  return { players };
}

export default function (data) {
  const me = data.players[(__VU - 1) % data.players.length];
  const headers = { "Content-Type": "application/json", Authorization: `Bearer ${me.token}` };
  const queueURL = `${BASE_URL}/api/v1/queue`;

  let room = null; // from MatchFound
  let queuedAt = 0;
  let joinSentAt = 0;
  let inRoom = false;
  let sawPartnerJoin = false;
  let sawPartnerLeave = false;

  const res = ws.connect(`${WS_URL}/ws?token=${me.token}`, {}, function (socket) {
    const leave = function () {
      if (inRoom) socket.send(JSON.stringify({ action: "leave", room }));
      socket.close();
    };

    socket.on("open", function () {
      // Queue only once the socket is open: MatchFound goes to the sockets a
      // player has at that moment and is not kept for later.
      const q = http.post(queueURL, JSON.stringify({ rank: RANK }), { headers, tags: LOAD });
      if (!check(q, { queued: (r) => r.status === 200 })) {
        socket.close();
        return;
      }
      queuedAt = Date.now();

      socket.setTimeout(function () {
        if (room) return;
        const d = http.del(queueURL, null, { headers, tags: LOAD, responseCallback: expectOkOrMissing });
        if (d.status !== 404) {
          socket.close(); // still queued and now removed: nobody to play with
          return;
        }
        // The matcher took the ticket in the meantime; its MatchFound is on
        // the way.
        socket.setTimeout(function () {
          if (!room) socket.close();
        }, MATCH_GRACE_MS);
      }, MATCH_TIMEOUT_MS);
    });

    socket.on("message", function (raw) {
      eventsReceived.add(1);
      let m;
      try {
        m = JSON.parse(raw);
      } catch (_) {
        return;
      }

      if (m.type === "MatchFound") {
        if (room) return;
        room = m.room;
        matchWait.add(Date.now() - queuedAt);
        joinSentAt = Date.now();
        socket.send(JSON.stringify({ action: "join", room }));
        socket.setTimeout(function () {
          if (!inRoom) socket.close(); // the join was never answered
        }, JOIN_TIMEOUT_MS);
        return;
      }
      if (!room || m.room !== room) return; // LeaderboardUpdated and the like

      if (m.type === "JoinRejected") {
        joinRejected.add(1);
        socket.close();
      } else if (m.type === "PlayerJoined") {
        if (m.data.player_id !== me.id) {
          sawPartnerJoin = true;
          return;
        }
        inRoom = true;
        joinTime.add(Date.now() - joinSentAt);
        // Stay 30-60 s like a real client. A room has two members and a
        // PlayerJoined reaches only those already inside, so exactly one of
        // the two sees the other arrive: the one who joined first. That one
        // leaves in the first half of the window (30-45 s) and the other in
        // the second (45-60 s), so the second sees the first go. Either way
        // each socket is sent one message about its partner.
        socket.setTimeout(function () {
          if (sawPartnerJoin) {
            leave();
            return;
          }
          socket.setTimeout(leave, 15000);
        }, 30000 + Math.random() * 15000);
      } else if (m.type === "PlayerLeft" && m.data.player_id !== me.id) {
        sawPartnerLeave = true;
      }
    });
  });

  check(res, { "ws upgrade 101": (r) => r && r.status === 101 });
  if (queuedAt) matched.add(room !== null);
  if (room) joined.add(inRoom);
  if (inRoom) roomFanout.add(sawPartnerJoin || sawPartnerLeave);
  sleep(1);
}
