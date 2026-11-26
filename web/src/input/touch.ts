// Mobile: a floating virtual stick (section 3.4). It appears where the
// finger first touches; the finger's offset from there is direction and
// throttle, 100 % at the stick's radius. No fixed button; one thumb, any
// orientation.

import type { Input } from "./input";
import { unit } from "./input";

export class TouchInput {
  private id: number | null = null;
  private ox = 0;
  private oy = 0;
  private x = 0;
  private y = 0;
  version = 0;
  private readonly base: HTMLDivElement;
  private readonly knob: HTMLDivElement;

  constructor(
    target: HTMLElement,
    private readonly radiusPx: () => number,
  ) {
    this.base = document.createElement("div");
    this.base.className = "stick-base";
    this.knob = document.createElement("div");
    this.knob.className = "stick-knob";
    document.body.append(this.base, this.knob);
    this.hide();

    target.addEventListener("pointerdown", (e) => {
      if (e.pointerType !== "touch" || this.id !== null) return;
      e.preventDefault();
      this.id = e.pointerId;
      this.ox = this.x = e.clientX;
      this.oy = this.y = e.clientY;
      this.version++;
      this.show();
    });
    target.addEventListener("pointermove", (e) => {
      if (e.pointerId !== this.id) return;
      e.preventDefault();
      this.x = e.clientX;
      this.y = e.clientY;
      this.version++;
      this.place();
    });
    const end = (e: PointerEvent) => {
      if (e.pointerId !== this.id) return;
      this.id = null;
      this.version++;
      this.hide();
    };
    target.addEventListener("pointerup", end);
    target.addEventListener("pointercancel", end);
  }

  /** read: the stick's input while a finger is down, else null. */
  read(): Input | null {
    if (this.id === null) return null;
    const u = unit(this.x - this.ox, this.y - this.oy);
    return { dx: u.dx, dy: u.dy, throttle: Math.min(1, u.len / this.radiusPx()) };
  }

  private show(): void {
    const r = this.radiusPx();
    this.base.style.width = this.base.style.height = `${2 * r}px`;
    this.base.style.display = this.knob.style.display = "block";
    this.place();
  }

  private place(): void {
    const r = this.radiusPx();
    this.base.style.transform = `translate(${this.ox - r}px, ${this.oy - r}px)`;
    const u = unit(this.x - this.ox, this.y - this.oy);
    const k = Math.min(u.len, r);
    const kx = this.ox + u.dx * k;
    const ky = this.oy + u.dy * k;
    this.knob.style.transform = `translate(${kx}px, ${ky}px) translate(-50%, -50%)`;
  }

  private hide(): void {
    this.base.style.display = this.knob.style.display = "none";
  }
}
