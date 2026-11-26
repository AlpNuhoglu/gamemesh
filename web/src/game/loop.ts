// The two clocks of the game: the requestAnimationFrame loop that predicts
// and draws, and the timer that sends a position every send_ms (section 3.1:
// one combined packet at the room's tick).
//
// Nothing here reads a clock or starts a timer of its own: the page passes
// them in (main.ts), as it passes the time to every other module under
// game/.

export interface LoopDeps {
  now(): number;
  raf(cb: () => void): unknown;
  setInterval(fn: () => void, ms: number): unknown;
  clearInterval(id: unknown): void;
}

export class Loop {
  private last: number | null = null;
  private sendTimer: unknown = null;
  private sendMs = 0;

  constructor(
    private readonly d: LoopDeps,
    private readonly onFrame: (now: number, dtMs: number) => void,
    private readonly onSend: (now: number) => void,
  ) {}

  /** start runs the frame loop; sending starts with setSendInterval. */
  start(): void {
    const tick = () => {
      const now = this.d.now();
      const dt = this.last === null ? 0 : now - this.last;
      this.last = now;
      this.onFrame(now, dt);
      this.d.raf(tick);
    };
    this.d.raf(tick);
  }

  /** setSendInterval sends every ms (snap.cfg.send_ms); 0 stops sending. */
  setSendInterval(ms: number): void {
    if (ms === this.sendMs) return;
    if (this.sendTimer !== null) this.d.clearInterval(this.sendTimer);
    this.sendTimer = null;
    this.sendMs = ms;
    if (ms > 0) this.sendTimer = this.d.setInterval(() => this.onSend(this.d.now()), ms);
  }
}
