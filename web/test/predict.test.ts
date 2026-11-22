import { describe, expect, test } from "vitest";
import vectors from "../../internal/ghosttag/testdata/validate_vectors.json";
import { validateMove, wrapPi, Predictor } from "../src/game/predict";
import { parseCfg, type Role } from "../src/net/protocol";

// The golden vectors of the movement validation (section 3.5): the Go side
// generates them from its own validateMove; this runs every case through the
// TypeScript port and compares every number.

const cfg = parseCfg(vectors.cfg);
const TOL = 1e-9;

describe("golden vectors", () => {
  test("the vector file's cfg has every snap.cfg field", () => {
    expect(cfg).not.toBeNull();
    expect(vectors.cases.length).toBeGreaterThan(40);
  });

  for (const c of vectors.cases) {
    test(c.name, () => {
      const out = validateMove(cfg!, c.in.prev, c.in.raw, c.in.dt_ms, c.in.role as Role, c.in.r);
      expect(Math.abs(out.pos.x - c.out.pos.x)).toBeLessThanOrEqual(TOL);
      expect(Math.abs(out.pos.y - c.out.pos.y)).toBeLessThanOrEqual(TOL);
      expect(Math.abs(out.theta - c.out.theta)).toBeLessThanOrEqual(TOL);
      expect(Math.abs(out.slack - c.out.slack)).toBeLessThanOrEqual(TOL);
      expect(out.clamped).toBe(c.out.clamped);
    });
  }
});

describe("wrapPi", () => {
  test("rounds half to even like math.Remainder", () => {
    expect(wrapPi(Math.PI)).toBeCloseTo(Math.PI, 12);
    expect(wrapPi(3 * Math.PI)).toBeCloseTo(-Math.PI, 12);
    expect(wrapPi(-Math.PI)).toBeCloseTo(-Math.PI, 12);
    expect(wrapPi(2 * Math.PI + 0.5)).toBeCloseTo(0.5, 12);
  });
});

describe("Predictor", () => {
  test("predicts with no slack: a step is at most the speed times the time", () => {
    const p = new Predictor();
    p.reset(0, 0, 0);
    // Full throttle straight ahead for one dt_max: the distance is v * dt and
    // no slack is added on top.
    p.step(cfg!, { dx: 1, dy: 0, throttle: 1 }, 40, "HUNTER", cfg!.r0);
    expect(p.x).toBeCloseTo((cfg!.v_hunter * 40) / 1000, 9);
    p.step(cfg!, { dx: 1, dy: 0, throttle: 1 }, 40, "HUNTER", cfg!.r0);
    expect(p.x).toBeCloseTo((2 * cfg!.v_hunter * 40) / 1000, 9);
  });

  test("a clamp moves the state at once and the drawing smoothly", () => {
    const p = new Predictor();
    p.reset(10, 0, 0);
    p.correct(0, 0, 0, 1000);
    expect(p.x).toBe(0);
    // Right at the clamp the drawing is still where it was: no teleport.
    expect(p.drawn(1000).x).toBeCloseTo(10, 9);
    const mid = p.drawn(1050).x;
    expect(mid).toBeGreaterThan(0);
    expect(mid).toBeLessThan(10);
    expect(p.drawn(5000).x).toBe(0);
  });
});
