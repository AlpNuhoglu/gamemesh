import { expect, test } from "vitest";
import vectors from "../../internal/ghosttag/testdata/validate_vectors.json";
import { Game } from "../src/game/game";
import { parseCfg, type Role, type RoomState, type Snap } from "../src/net/protocol";

// A match as the client plays it, driven by messages and times the test
// chooses.

const cfg = parseCfg(vectors.cfg)!;

function snap(state: RoomState, myRole: Role, remMs: number): Snap {
  const other: Role = myRole === "HUNTER" ? "RUNNER" : "HUNTER";
  const at = (r: Role) => (r === "HUNTER" ? { x: -225, y: 0, theta: 0 } : { x: 225, y: 0, theta: Math.PI });
  return {
    t: "snap", state, sts: 0, rem_ms: remMs, r: cfg.r0, cfg,
    players: [
      { id: "me", name: "Me", role: myRole, connected: true, ...at(myRole) },
      { id: "them", name: "Them", role: other, connected: true, ...at(other) },
    ],
  };
}

function pos(sts: number, x: number, y = 0) {
  return { t: "pos" as const, from: "them", sts, x, y, vx: 0, vy: 0 };
}

test("as RUNNER the hunter is never drawn live, only from trail_ms ago", () => {
  const g = new Game("me", "room");
  g.onMessage(snap("COUNTDOWN_1", "RUNNER", 2000), 0);
  expect(g.view(0)!.opponent).toBeNull(); // not even standing on its spawn
  g.onMessage(snap("HALF_1_ACTIVE", "RUNNER", cfg.t_half_ms), 2000);
  // The hunter moves +x one unit per 40 ms packet.
  for (let i = 1; i <= 100; i++) g.onMessage(pos(2000 + i * 40, -225 + i), 2000 + i * 40);
  const now = 2000 + 100 * 40;
  const v = g.view(now)!;
  expect(v.opponent).toBeNull();
  // The head is the hunter of trail_ms ago, not the newest packet (-125).
  expect(v.trail[0]!.x).toBeCloseTo(-225 + (now - cfg.trail_ms - 2000) / 40, 9);
  expect(v.hunterAt).toMatchObject({ x: v.trail[0]!.x, y: v.trail[0]!.y });
});

test("as RUNNER, before trail_ms into the half the hunter shows on its spawn point", () => {
  const g = new Game("me", "room");
  g.onMessage(snap("COUNTDOWN_1", "RUNNER", 2000), 0);
  g.onMessage(snap("HALF_1_ACTIVE", "RUNNER", cfg.t_half_ms), 2000);
  g.onMessage(pos(2040, -200), 2040);
  // 500 ms into the half: the hunter of 1500 ms before the half, at spawn.
  expect(g.view(2500)!.trail[0]).toMatchObject({ x: -225, y: 0 });
});

test("as HUNTER the runner is drawn live, interp_ms behind", () => {
  const g = new Game("me", "room");
  g.onMessage(snap("HALF_1_ACTIVE", "HUNTER", cfg.t_half_ms), 0);
  g.onMessage({ ...pos(1000, 225), from: "them" }, 1000);
  g.onMessage({ ...pos(1040, 235), from: "them" }, 1040);
  const v = g.view(1040 + cfg.interp_ms - 20)!; // render time 1020: half way
  expect(v.trail).toEqual([]);
  expect(v.opponent!.x).toBeCloseTo(230, 9);
  expect(v.opponent!.mode).toBe("interp");
});

test("own movement only in a half; packets carry an increasing seq", () => {
  const g = new Game("me", "room");
  g.onMessage(snap("COUNTDOWN_1", "HUNTER", 2000), 0);
  expect(g.frame(10, 16, { dx: 1, dy: 0, throttle: 1 })).toBe(false);
  expect(g.packet(10)).toBeNull();
  g.onMessage(snap("HALF_1_ACTIVE", "HUNTER", cfg.t_half_ms), 2000);
  expect(g.frame(2016, 16, { dx: 1, dy: 0, throttle: 1 })).toBe(true);
  const a = g.packet(2040)!;
  const b = g.packet(2080)!;
  expect(a.x).toBeCloseTo(-225 + (cfg.v_hunter * 16) / 1000, 9);
  expect(b.seq).toBe(a.seq + 1);
});

test("a clamp corrects the state, and the drawing glides", () => {
  const g = new Game("me", "room");
  g.onMessage(snap("HALF_1_ACTIVE", "HUNTER", cfg.t_half_ms), 0);
  g.onMessage({ t: "clamp", x: -200, y: 10, theta: 0 }, 100);
  expect(g.pred.x).toBe(-200);
  expect(g.view(100)!.me.x).toBeCloseTo(-225, 9);
});

test("the arena shrinks with the half's clock", () => {
  const g = new Game("me", "room");
  g.onMessage(snap("HALF_1_ACTIVE", "HUNTER", cfg.t_half_ms), 0);
  expect(g.view(cfg.t_half_ms - cfg.t_shrink_ms)!.radius).toBe(cfg.r0);
  expect(g.view(cfg.t_half_ms)!.radius).toBeCloseTo(cfg.r_min, 9);
});
