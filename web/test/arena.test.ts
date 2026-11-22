import { expect, test } from "vitest";
import vectors from "../../internal/ghosttag/testdata/validate_vectors.json";
import { arenaRadius } from "../src/game/arena";
import { parseCfg } from "../src/net/protocol";

// Expected values worked out by hand from arenaRadius in
// internal/ghosttag/validate.go, with the vector file's cfg (r0 500,
// r_min 280, t_half 45 s, t_shrink 15 s): R0 until t_half − t_shrink = 30 s,
// then linear to r_min at 45 s, and r_min after.
const cfg = parseCfg(vectors.cfg)!;

test.each([
  [0, 500],
  [29_999, 500],
  [30_000, 500],
  [30_001, 500 - 220 / 15_000],
  [33_750, 445],
  [37_500, 390],
  [44_000, 500 - 220 * (14 / 15)],
  [45_000, 280],
  [60_000, 280],
])("R(%d ms) = %d", (elapsed, want) => {
  expect(arenaRadius(cfg, elapsed)).toBeCloseTo(want, 9);
});

test("no shrink window: the radius stays r0", () => {
  expect(arenaRadius({ ...cfg, t_shrink_ms: 0 }, 44_000)).toBe(cfg.r0);
});
