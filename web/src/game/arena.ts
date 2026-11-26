// R(t): the shrinking arena (section 1.2), a port of arenaRadius in
// internal/ghosttag/validate.go. snap is event-based, so between two snaps
// the client computes the radius itself from the half's elapsed time.

import type { Cfg } from "../net/protocol";

/** arenaRadius is R(t) at elapsedMs of active time into a half. */
export function arenaRadius(cfg: Cfg, elapsedMs: number): number {
  const start = cfg.t_half_ms - cfg.t_shrink_ms;
  if (elapsedMs <= start || cfg.t_shrink_ms <= 0) return cfg.r0;
  const f = Math.min(1, (elapsedMs - start) / cfg.t_shrink_ms);
  return cfg.r0 - (cfg.r0 - cfg.r_min) * f;
}
