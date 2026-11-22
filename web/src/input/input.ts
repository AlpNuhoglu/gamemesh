// The one output every control produces: a direction and a throttle
// (predict.ts's Input), plus a version that changes whenever the player's
// intent changed, for the developer probe.

export type { Input } from "../game/predict";

export const NO_INPUT = { dx: 0, dy: 0, throttle: 0 } as const;

/** unit returns (x, y) as a unit vector, or (0, 0). */
export function unit(x: number, y: number): { dx: number; dy: number; len: number } {
  const len = Math.hypot(x, y);
  return len === 0 ? { dx: 0, dy: 0, len: 0 } : { dx: x / len, dy: y / len, len };
}
