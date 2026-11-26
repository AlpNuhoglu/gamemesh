// The arena on one square canvas (section 3.4). Plain shapes: the arena, the
// two characters, the capture ring around the hunter, and, as RUNNER, the
// hunter's fading trail instead of the hunter.

import type { View } from "../game/game";
import type { Vec } from "../game/predict";
import { TAIL_MS } from "../game/trail";

const C = {
  bg: "#0b0f1a",
  outside: "#0e1322",
  arena: "#151c30",
  r0: "#24304d",
  edge: "#6b7fb0",
  hunter: "#ff6b4a",
  runner: "#4ad8ff",
  me: "#ffffff",
  text: "#c9d1e6",
};

function roleColor(role: string): string {
  return role === "HUNTER" ? C.hunter : C.runner;
}

export class CanvasView {
  private readonly ctx: CanvasRenderingContext2D;
  private side = 0;
  private scale = 1;

  constructor(readonly el: HTMLCanvasElement) {
    const ctx = el.getContext("2d");
    if (ctx === null) throw new Error("no 2d context");
    this.ctx = ctx;
  }

  /** resize makes the canvas side CSS pixels square. */
  resize(side: number): void {
    const dpr = window.devicePixelRatio || 1;
    this.side = side;
    this.el.style.width = this.el.style.height = `${side}px`;
    this.el.width = this.el.height = Math.round(side * dpr);
    this.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  }

  /** toClient is where an arena point is on the page, in client pixels. */
  toClient(view: View, p: Vec): Vec {
    const r = this.el.getBoundingClientRect();
    const s = this.unitPx(view);
    return { x: r.left + r.width / 2 + p.x * s, y: r.top + r.height / 2 + p.y * s };
  }

  /** unitPx is CSS pixels per arena unit. */
  unitPx(view: View): number {
    return this.side / 2 / (view.cfg.r0 * 1.04);
  }

  draw(view: View | null): void {
    const ctx = this.ctx;
    const side = this.side;
    ctx.fillStyle = C.bg;
    ctx.fillRect(0, 0, side, side);
    if (view === null) return;
    const s = (this.scale = this.unitPx(view));
    const cx = side / 2;
    const cy = side / 2;
    const X = (x: number) => cx + x * s;
    const Y = (y: number) => cy + y * s;
    const body = view.cfg.d_capture * 0.4; // drawn radius; D_capture is ~2.5 of it (section 8)

    // The arena: the starting circle faint, the current R(t) bright.
    circle(ctx, cx, cy, view.cfg.r0 * s, C.outside, null);
    circle(ctx, cx, cy, view.radius * s, C.arena, C.edge, 2);
    if (view.radius < view.cfg.r0) circle(ctx, cx, cy, view.cfg.r0 * s, null, C.r0, 1);

    // The capture ring around the hunter, where the hunter is drawn.
    if (view.hunterAt !== null) {
      ctx.setLineDash([4, 4]);
      circle(ctx, X(view.hunterAt.x), Y(view.hunterAt.y), view.cfg.d_capture * s, null, C.hunter, 1);
      ctx.setLineDash([]);
    }

    // As RUNNER: the hunter's trail, the point from trail_ms ago at its head.
    if (view.trail.length > 0) {
      for (let i = view.trail.length - 1; i >= 1; i--) {
        const a = view.trail[i]!;
        const b = view.trail[i - 1]!;
        ctx.globalAlpha = Math.max(0, 1 - a.age / TAIL_MS) * 0.8;
        ctx.strokeStyle = C.hunter;
        ctx.lineWidth = Math.max(1, body * s * 0.9);
        ctx.lineCap = "round";
        ctx.beginPath();
        ctx.moveTo(X(a.x), Y(a.y));
        ctx.lineTo(X(b.x), Y(b.y));
        ctx.stroke();
      }
      ctx.globalAlpha = 0.55;
      const h = view.trail[0]!;
      circle(ctx, X(h.x), Y(h.y), body * s, C.hunter, null);
      ctx.globalAlpha = 1;
    }

    // The opponent, live (as HUNTER, or outside a half).
    if (view.opponent !== null) {
      const o = view.opponent;
      circle(ctx, X(o.x), Y(o.y), body * s, roleColor(o.role), null);
      label(ctx, X(o.x), Y(o.y) + body * s + 12, o.name || "opponent");
    }

    // The own character, with its heading.
    const m = view.me;
    circle(ctx, X(m.x), Y(m.y), body * s, roleColor(m.role), C.me, 2);
    ctx.strokeStyle = C.me;
    ctx.lineWidth = 2;
    ctx.beginPath();
    ctx.moveTo(X(m.x), Y(m.y));
    ctx.lineTo(X(m.x) + Math.cos(m.theta) * body * s * 1.6, Y(m.y) + Math.sin(m.theta) * body * s * 1.6);
    ctx.stroke();
    label(ctx, X(m.x), Y(m.y) + body * s + 12, "you");
  }

  /** lastScale is the scale of the last frame drawn. */
  get lastScale(): number {
    return this.scale;
  }
}

function circle(
  ctx: CanvasRenderingContext2D, x: number, y: number, r: number,
  fill: string | null, stroke: string | null, width = 1,
): void {
  ctx.beginPath();
  ctx.arc(x, y, Math.max(0, r), 0, Math.PI * 2);
  if (fill !== null) {
    ctx.fillStyle = fill;
    ctx.fill();
  }
  if (stroke !== null) {
    ctx.strokeStyle = stroke;
    ctx.lineWidth = width;
    ctx.stroke();
  }
}

function label(ctx: CanvasRenderingContext2D, x: number, y: number, text: string): void {
  ctx.fillStyle = C.text;
  ctx.font = "12px system-ui, sans-serif";
  ctx.textAlign = "center";
  ctx.fillText(text, x, y);
}
