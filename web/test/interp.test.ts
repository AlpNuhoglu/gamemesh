import { beforeEach, describe, expect, test } from "vitest";
import { InterpBuffer } from "../src/game/interp";

// The buffer is driven with times the test chooses; nothing reads a clock.
const cfg = { interp_ms: 60, dr_ms: 80 };

let b: InterpBuffer;
beforeEach(() => {
  b = new InterpBuffer();
  // Two packets 40 ms apart, moving +x at 250 units/s.
  b.push({ t: 1000, x: 0, y: 0, vx: 250, vy: 0 });
  b.push({ t: 1040, x: 10, y: 20, vx: 250, vy: 0 });
});

describe("interpolation", () => {
  test("empty buffer draws nothing", () => {
    expect(new InterpBuffer().sample(5000, cfg).mode).toBe("empty");
  });

  test("before the first packet: held at the first", () => {
    // render time 1000 - 1 = 999 < first.
    const d = b.sample(1059, cfg);
    expect(d).toEqual({ x: 0, y: 0, mode: "before" });
  });

  test("exactly on a packet: that packet", () => {
    expect(b.sample(1060, cfg)).toEqual({ x: 0, y: 0, mode: "interp" });
    expect(b.sample(1100, cfg)).toEqual({ x: 10, y: 20, mode: "interp" });
  });

  test("between two packets: linear, interp_ms behind", () => {
    // render time 1010: a quarter of the way.
    const d = b.sample(1070, cfg);
    expect(d.mode).toBe("interp");
    expect(d.x).toBeCloseTo(2.5, 9);
    expect(d.y).toBeCloseTo(5, 9);
  });

  test("an out-of-order packet is dropped", () => {
    expect(b.push({ t: 1020, x: 99, y: 99, vx: 0, vy: 0 })).toBe(false);
    expect(b.sample(1080, cfg).x).toBeCloseTo(5, 9);
  });
});

describe("dead reckoning", () => {
  test("extrapolates from vx, vy while the newest is up to dr_ms late", () => {
    // render time 1040 + 40: 40 ms past the newest.
    const d = b.sample(1140, cfg);
    expect(d.mode).toBe("extrap");
    expect(d.x).toBeCloseTo(10 + 250 * 0.04, 9);
    expect(d.y).toBeCloseTo(20, 9);
    // At exactly dr_ms.
    expect(b.sample(1180, cfg).x).toBeCloseTo(10 + 250 * 0.08, 9);
  });

  test("then holds at the dr_ms point", () => {
    const later = b.sample(1181, cfg);
    expect(later.mode).toBe("hold");
    expect(later.x).toBeCloseTo(10 + 250 * 0.08, 9);
    expect(b.sample(9000, cfg).x).toBeCloseTo(10 + 250 * 0.08, 9);
  });
});

test("prune keeps the sample the render time needs", () => {
  b.push({ t: 1080, x: 20, y: 0, vx: 0, vy: 0 });
  b.prune(1110, cfg); // render time 1050: needs 1040 and 1080
  expect(b.size).toBe(2);
  expect(b.sample(1110, cfg).x).toBeCloseTo(12.5, 9);
});
