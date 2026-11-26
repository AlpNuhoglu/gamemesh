// Own movement: the server's validation formula (design document, sections
// 1.2 and 3.5), and the predictor that moves the own character with it.
//
// validateMove is a port of validateMove in internal/ghosttag/validate.go for
// a player who has a previous validated packet. It is checked against every
// case of internal/ghosttag/testdata/validate_vectors.json; if you change it,
// the Go file is the reference.

import type { Cfg, Role } from "../net/protocol";

export interface Vec {
  x: number;
  y: number;
}

export interface Prev {
  x: number;
  y: number;
  theta: number;
  slack: number;
}

export interface MoveResult {
  pos: Vec;
  theta: number;
  slack: number;
  clamped: boolean;
}

// How far a validated position may lie from the raw one and still count as
// unclipped. It is the Go side's clipEpsilon: float rounding in the polar
// step, not a game rule, so it is not in snap.cfg.
const CLIP_EPSILON = 1e-6;

const TWO_PI = 2 * Math.PI;

/** wrapPi maps an angle into [−π, π], as Go's math.Remainder(a, 2π) does. */
export function wrapPi(a: number): number {
  const q = a / TWO_PI;
  let n = Math.round(q);
  // math.Remainder rounds half to even; Math.round rounds half up.
  if (Math.abs(q - Math.trunc(q)) === 0.5) n = 2 * Math.round(q / 2);
  return a - n * TWO_PI;
}

function intoArena(p: Vec, r: number): { p: Vec; scaled: boolean } {
  const n = Math.hypot(p.x, p.y);
  if (n > r) {
    const k = r / n;
    return { p: { x: p.x * k, y: p.y * k }, scaled: true };
  }
  return { p: { x: p.x, y: p.y }, scaled: false };
}

/**
 * validateMove: from prev, dtMs later, a player in role asks to be at raw in
 * an arena of radius r. Same shape as the golden vectors.
 */
export function validateMove(cfg: Cfg, prev: Prev, raw: Vec, dtMs: number, role: Role, r: number): MoveResult {
  const dx = raw.x - prev.x;
  const dy = raw.y - prev.y;
  const dist = Math.hypot(dx, dy);
  if (!Number.isFinite(dist)) {
    // The server refuses the packet; the client never produces one.
    return { pos: { x: prev.x, y: prev.y }, theta: prev.theta, slack: prev.slack, clamped: false };
  }

  // 1. The time step: real elapsed time, capped at dt_max, never raised.
  const dt = Math.max(0, Math.min(cfg.dt_max_ms / 1000, dtMs / 1000));
  const base = role === "RUNNER" ? cfg.v_runner : cfg.v_hunter;

  if (dist === 0 || dist < cfg.move_eps) {
    // Standing still: no direction is derived, the unused budget is slack.
    const slack = Math.min(cfg.slack_max, prev.slack + base * dt);
    const a = intoArena(prev, r);
    return { pos: a.p, theta: prev.theta, slack, clamped: a.scaled };
  }

  // 2. Direction; the runner's turn rate and turn penalty.
  const thetaRaw = Math.atan2(dy, dx);
  let theta = thetaRaw;
  let allowed = base;
  if (role === "RUNNER") {
    const limit = cfg.omega_max * dt;
    const turn = Math.max(-limit, Math.min(limit, wrapPi(thetaRaw - prev.theta)));
    theta = wrapPi(prev.theta + turn);
    allowed = cfg.v_runner * (1 - (cfg.alpha * Math.abs(turn)) / Math.PI);
  }

  // 3. Distance against this step's budget plus the slack, then the arena.
  const budget = allowed * dt;
  const step = Math.min(dist, budget + prev.slack);
  const slack = Math.max(0, Math.min(cfg.slack_max, prev.slack + budget - step));
  const a = intoArena({ x: prev.x + step * Math.cos(theta), y: prev.y + step * Math.sin(theta) }, r);
  const off = Math.hypot(a.p.x - raw.x, a.p.y - raw.y);
  return { pos: a.p, theta, slack, clamped: off > CLIP_EPSILON };
}

/** Input is the common output of every control: a direction and a throttle. */
export interface Input {
  /** Direction, a unit vector (or 0, 0 for none). Arena axes. */
  dx: number;
  dy: number;
  /** Throttle in [0, 1]. */
  throttle: number;
}

// How long a clamp correction is blended in, in milliseconds. A presentation
// choice (how the correction looks), not a rule of the game: the state the
// client predicts from is corrected at once, only the drawing glides.
const CLAMP_BLEND_MS = 150;

/**
 * Predictor moves the own character. It never reconciles: the only thing
 * that corrects it is a clamp from the server, and then smoothly. It predicts
 * every step with a slack of 0 (section 3.5): the slack budget is the
 * server's, for network jitter.
 */
export class Predictor {
  x = 0;
  y = 0;
  theta = 0;
  /** The drawn position's offset from (x, y) when the last clamp landed. */
  private offX = 0;
  private offY = 0;
  private blendFrom = 0;
  clamps = 0;
  /** How far the last clamp moved the state, in arena units. */
  lastCorrection = 0;

  /** reset places the character, as at a spawn: no blending. */
  reset(x: number, y: number, theta: number): void {
    this.x = x;
    this.y = y;
    this.theta = theta;
    this.offX = 0;
    this.offY = 0;
  }

  /**
   * step moves the character for dtMs of input in role, in an arena of radius
   * r. It returns whether the position changed.
   */
  step(cfg: Cfg, input: Input, dtMs: number, role: Role, r: number): boolean {
    const v = role === "RUNNER" ? cfg.v_runner : cfg.v_hunter;
    const dt = Math.max(0, Math.min(cfg.dt_max_ms, dtMs)) / 1000;
    const len = Math.min(1, Math.max(0, input.throttle)) * v * dt;
    const raw = { x: this.x + input.dx * len, y: this.y + input.dy * len };
    const res = validateMove(cfg, { x: this.x, y: this.y, theta: this.theta, slack: 0 }, raw, dtMs, role, r);
    const moved = res.pos.x !== this.x || res.pos.y !== this.y;
    this.x = res.pos.x;
    this.y = res.pos.y;
    this.theta = res.theta;
    return moved;
  }

  /**
   * correct applies a clamp at now: the state jumps to the server's position
   * (so the next packets are measured from where the server has us), the
   * drawing blends from where it was.
   */
  correct(x: number, y: number, theta: number, now: number): void {
    const drawn = this.drawn(now);
    this.lastCorrection = Math.hypot(x - this.x, y - this.y);
    this.x = x;
    this.y = y;
    this.theta = theta;
    this.offX = drawn.x - x;
    this.offY = drawn.y - y;
    this.blendFrom = now;
    this.clamps++;
  }

  /** drawn is where the character is drawn at now. */
  drawn(now: number): Vec {
    const k = 1 - Math.min(1, Math.max(0, (now - this.blendFrom) / CLAMP_BLEND_MS));
    return { x: this.x + this.offX * k, y: this.y + this.offY * k };
  }
}
