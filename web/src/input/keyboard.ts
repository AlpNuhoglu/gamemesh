// Desktop alternative: WASD and the arrow keys, at full throttle.

import type { Input } from "./input";
import { unit } from "./input";

const KEYS: Record<string, [number, number]> = {
  KeyW: [0, -1], ArrowUp: [0, -1],
  KeyS: [0, 1], ArrowDown: [0, 1],
  KeyA: [-1, 0], ArrowLeft: [-1, 0],
  KeyD: [1, 0], ArrowRight: [1, 0],
};

export class KeyboardInput {
  private down = new Set<string>();
  version = 0;

  constructor(target: Window) {
    target.addEventListener("keydown", (e) => {
      if (!(e.code in KEYS)) return;
      e.preventDefault();
      if (!this.down.has(e.code)) this.version++;
      this.down.add(e.code);
    });
    target.addEventListener("keyup", (e) => {
      if (this.down.delete(e.code)) this.version++;
    });
    target.addEventListener("blur", () => {
      if (this.down.size > 0) this.version++;
      this.down.clear();
    });
  }

  /** read: the input while a key is held, else null. */
  read(): Input | null {
    if (this.down.size === 0) return null;
    let x = 0;
    let y = 0;
    for (const k of this.down) {
      const v = KEYS[k];
      if (v !== undefined) {
        x += v[0];
        y += v[1];
      }
    }
    const u = unit(x, y);
    return { dx: u.dx, dy: u.dy, throttle: u.len === 0 ? 0 : 1 };
  }
}
