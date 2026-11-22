// Desktop: the pointer sets direction and throttle, with no click (section
// 3.4). The throttle is the pointer's distance from the own character, 0 at
// the character and 100 % at fullPx or more.

import type { Input } from "./input";
import { unit } from "./input";

export class MouseInput {
  private x = 0;
  private y = 0;
  private inside = false;
  version = 0;

  constructor(target: HTMLElement) {
    target.addEventListener("pointermove", (e) => {
      if (e.pointerType !== "mouse") return;
      this.x = e.clientX;
      this.y = e.clientY;
      this.inside = true;
      this.version++;
    });
    target.addEventListener("pointerleave", (e) => {
      if (e.pointerType !== "mouse") return;
      this.inside = false;
      this.version++;
    });
  }

  /** read: the input for a character drawn at (cx, cy) in client pixels. */
  read(cx: number, cy: number, fullPx: number): Input | null {
    if (!this.inside) return null;
    const u = unit(this.x - cx, this.y - cy);
    return { dx: u.dx, dy: u.dy, throttle: Math.min(1, u.len / fullPx) };
  }
}
