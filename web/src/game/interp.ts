// The opponent as drawn (section 3.2): interp_ms behind, linearly
// interpolated between the two packets around that time; when the newest
// packet is older than that, dead-reckoned from its vx, vy for at most
// dr_ms, then held.
//
// The buffer is keyed by a time the caller chooses, in local milliseconds.
// The game keys it by the packet's sts mapped onto the local clock (sts is
// for ordering and jitter correction, section 6), so packets keep the
// spacing the server received them at.

export interface Sample {
  t: number;
  x: number;
  y: number;
  vx: number;
  vy: number;
}

export type InterpMode = "empty" | "before" | "interp" | "extrap" | "hold";

export interface Drawn {
  x: number;
  y: number;
  mode: InterpMode;
}

export interface InterpCfg {
  interp_ms: number;
  dr_ms: number;
}

export class InterpBuffer {
  private s: Sample[] = [];

  get size(): number {
    return this.s.length;
  }

  /** newest is the newest sample, or undefined. */
  get newest(): Sample | undefined {
    return this.s[this.s.length - 1];
  }

  clear(): void {
    this.s = [];
  }

  /** push adds a sample; one not after the newest is out of order and dropped. */
  push(p: Sample): boolean {
    const last = this.newest;
    if (last !== undefined && p.t <= last.t) return false;
    this.s.push(p);
    return true;
  }

  /** sample is the opponent's drawn position at local time now. */
  sample(now: number, cfg: InterpCfg): Drawn {
    const s = this.s;
    const first = s[0];
    const last = s[s.length - 1];
    if (first === undefined || last === undefined) return { x: 0, y: 0, mode: "empty" };
    const rt = now - cfg.interp_ms;
    if (rt < first.t) return { x: first.x, y: first.y, mode: "before" };
    if (rt > last.t) {
      const ahead = rt - last.t;
      const k = Math.min(ahead, cfg.dr_ms) / 1000;
      return { x: last.x + last.vx * k, y: last.y + last.vy * k, mode: ahead > cfg.dr_ms ? "hold" : "extrap" };
    }
    // The newest sample at or before rt, and the one after it.
    let i = s.length - 1;
    while (i > 0 && s[i]!.t > rt) i--;
    const a = s[i]!;
    const b = s[i + 1];
    if (b === undefined || a.t === rt) return { x: a.x, y: a.y, mode: "interp" };
    const f = (rt - a.t) / (b.t - a.t);
    return { x: a.x + (b.x - a.x) * f, y: a.y + (b.y - a.y) * f, mode: "interp" };
  }

  /**
   * prune forgets samples no longer needed at now: everything older than the
   * newest sample at or before the render time.
   */
  prune(now: number, cfg: InterpCfg): void {
    const rt = now - cfg.interp_ms;
    let i = 0;
    while (i + 1 < this.s.length && this.s[i + 1]!.t <= rt) i++;
    if (i > 0) this.s.splice(0, i);
  }
}
