// The runner's view of the hunter (section 3.3): the hunter is never drawn
// live. Its positions are kept by local receive time (not aligned with sts
// or any other clock), and only the point from trail_ms ago is drawn, with a
// fading tail behind it.

import type { Vec } from "./predict";

// How much older than trail_ms the drawn tail reaches, in milliseconds. It is
// how long the tail looks, a presentation choice; the buffer holds
// trail_ms plus this (2.5 s with the default trail of 2 s, section 3.3).
export const TAIL_MS = 500;

interface Point {
  t: number;
  x: number;
  y: number;
}

export interface TailPoint extends Vec {
  /** age past trail_ms, 0 at the head, TAIL_MS at the end of the tail. */
  age: number;
}

export class Trail {
  private p: Point[] = [];

  constructor(private trailMs: number) {}

  get size(): number {
    return this.p.length;
  }

  /** setDelay changes trail_ms (a new snap.cfg). */
  setDelay(trailMs: number): void {
    this.trailMs = trailMs;
  }

  clear(): void {
    this.p = [];
  }

  /** push records the hunter at (x, y), received at local time t. */
  push(t: number, x: number, y: number): void {
    const last = this.p[this.p.length - 1];
    if (last !== undefined && t < last.t) return;
    this.p.push({ t, x, y });
    this.forget(t);
  }

  /** forget drops what is older than the buffer's length at now. */
  forget(now: number): void {
    const keep = now - this.trailMs - TAIL_MS;
    let i = 0;
    while (i < this.p.length && this.p[i]!.t < keep) i++;
    if (i > 0) this.p.splice(0, i);
  }

  /** at is the hunter's position at local time t, interpolated; null without data that old. */
  private at(t: number): Vec | null {
    const p = this.p;
    const first = p[0];
    if (first === undefined || first.t > t) return null;
    let i = p.length - 1;
    while (i > 0 && p[i]!.t > t) i--;
    const a = p[i]!;
    const b = p[i + 1];
    if (b === undefined || a.t === t) return { x: a.x, y: a.y };
    const f = (t - a.t) / (b.t - a.t);
    return { x: a.x + (b.x - a.x) * f, y: a.y + (b.y - a.y) * f };
  }

  /** head is the drawn hunter at now: where it was trail_ms ago, or null. */
  head(now: number): Vec | null {
    return this.at(now - this.trailMs);
  }

  /** tail is the fading tail behind the head at now, head first. */
  tail(now: number): TailPoint[] {
    const h = now - this.trailMs;
    const out: TailPoint[] = [];
    const head = this.at(h);
    if (head === null) return out;
    out.push({ ...head, age: 0 });
    for (let i = this.p.length - 1; i >= 0; i--) {
      const q = this.p[i]!;
      if (q.t >= h) continue;
      if (h - q.t > TAIL_MS) break;
      out.push({ x: q.x, y: q.y, age: h - q.t });
    }
    return out;
  }
}
