import { beforeEach, expect, test } from "vitest";
import { GameSocket } from "../src/net/socket";
import { FakeWS, ManualTimers, settle } from "./fakes";

// The socket against a fake WebSocket and a fake identity: what is
// constructed, what is sent, and what the page is told, recorded.

let tokens: string[];
let renewals: number;
let timers: ManualTimers;
let trouble: boolean[];
let opens: number;
let sock: GameSocket;

beforeEach(() => {
  FakeWS.all = [];
  tokens = ["token-A"];
  renewals = 0;
  timers = new ManualTimers();
  trouble = [];
  opens = 0;
  sock = new GameSocket({
    WebSocket: FakeWS,
    url: (t) => `/ws?token=${t}`,
    identity: {
      token: async () => tokens[tokens.length - 1]!,
      renew: async () => {
        renewals++;
        tokens.push(`token-${String.fromCharCode(65 + tokens.length)}`);
      },
    },
    timers,
    random: () => 0.5,
    now: () => 1000,
  });
  sock.onTrouble = (t) => trouble.push(t);
  sock.onOpen = () => opens++;
});

const joins = (ws: FakeWS) => ws.sent.map((s) => JSON.parse(s)).filter((m) => m.action === "join");

test("1008: renew, reconnect with the new token, rejoin the room, nothing shown", async () => {
  sock.connect();
  await settle();
  const first = FakeWS.all[0]!;
  expect(first.url).toBe("/ws?token=token-A");
  first.open();
  sock.join("room-1");
  expect(joins(first)).toEqual([{ action: "join", room: "room-1" }]);

  first.serverClose(1008, "token expired");
  await settle();
  expect(renewals).toBe(1);
  // Reconnected at once, not on a timer, and with the renewed token.
  expect(FakeWS.all).toHaveLength(2);
  const second = FakeWS.all[1]!;
  expect(second.url).toBe("/ws?token=token-B");
  second.open();
  expect(joins(second)).toEqual([{ action: "join", room: "room-1" }]);
  expect(sock.isOpen).toBe(true);
  // No user-visible state in between: the page was never told of trouble.
  expect(trouble).toEqual([]);
  expect(opens).toBe(2);
});

test("1001: reconnect after a random delay, with the same token", async () => {
  sock.connect();
  await settle();
  FakeWS.all[0]!.open();
  FakeWS.all[0]!.serverClose(1001, "server shutting down");
  await settle();
  expect(FakeWS.all).toHaveLength(1); // not yet
  expect(timers.timeouts()).toEqual([250 + 0.5 * 2000]);
  timers.fireTimeouts();
  await settle();
  expect(FakeWS.all).toHaveLength(2);
  expect(FakeWS.all[1]!.url).toBe("/ws?token=token-A");
  expect(renewals).toBe(0);
  expect(trouble).toEqual([]);
});

test("a dropped connection reconnects quickly and rejoins", async () => {
  sock.connect();
  await settle();
  FakeWS.all[0]!.open();
  sock.join("room-2");
  FakeWS.all[0]!.serverClose(1006);
  await settle();
  expect(timers.timeouts()).toEqual([100]);
  timers.fireTimeouts();
  await settle();
  FakeWS.all[1]!.open();
  expect(joins(FakeWS.all[1]!)).toEqual([{ action: "join", room: "room-2" }]);
  expect(renewals).toBe(0);
});

test("a handshake that keeps failing renews the token and, later, tells the page", async () => {
  sock.connect();
  await settle();
  for (let i = 0; i < 3; i++) {
    FakeWS.all[i]!.serverClose(1006); // never opened
    await settle();
    timers.fireTimeouts();
    await settle();
  }
  expect(renewals).toBe(1);
  expect(trouble).toEqual([true]);
  FakeWS.all[3]!.open();
  expect(trouble).toEqual([true, false]);
});

test("pong gives the round trip", async () => {
  let now = 1000;
  const s = new GameSocket({
    WebSocket: FakeWS, url: (t) => t, timers, random: () => 0, now: () => now,
    identity: { token: async () => "t", renew: async () => {} },
  });
  s.connect();
  await settle();
  const ws = FakeWS.all[0]!;
  ws.open();
  s.join("r");
  const ping = ws.sent.map((x) => JSON.parse(x)).find((m) => m.action === "relay");
  expect(ping).toEqual({ action: "relay", room: "r", data: { t: "ping", cts: 1000 } });
  now = 1042;
  ws.receive({ type: "Game", room: "r", data: { t: "pong", cts: 1000, sts: 5 } });
  expect(s.rtt).toBe(42);
});
