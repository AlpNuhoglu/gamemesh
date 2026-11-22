// The top strip (section 3.4): [role badge] [00:34] [● ○], and who the
// opponent is. Plain DOM; text changes only when it differs.

import type { Role } from "../net/protocol";

function el<K extends keyof HTMLElementTagNameMap>(tag: K, cls: string, parent: HTMLElement): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag);
  e.className = cls;
  parent.append(e);
  return e;
}

function set(e: HTMLElement, text: string): void {
  if (e.textContent !== text) e.textContent = text;
}

export interface HudState {
  role: Role | "";
  remainingMs: number | null;
  /** half: the half being played (1 or 2); done: halves finished. */
  half: 1 | 2;
  done: number;
  opponent: string;
  note: string;
}

export class Hud {
  private readonly badge: HTMLSpanElement;
  private readonly clock: HTMLSpanElement;
  private readonly halves: HTMLSpanElement[];
  private readonly vs: HTMLSpanElement;
  private readonly note: HTMLSpanElement;

  constructor(root: HTMLElement) {
    this.badge = el("span", "badge", root);
    this.clock = el("span", "clock", root);
    const h = el("span", "halves", root);
    this.halves = [el("span", "half", h), el("span", "half", h)];
    this.vs = el("span", "vs", root);
    this.note = el("span", "note", root);
    this.update({ role: "", remainingMs: null, half: 1, done: 0, opponent: "", note: "" });
  }

  update(s: HudState): void {
    set(this.badge, s.role === "" ? "GHOST TAG" : s.role);
    this.badge.className = `badge ${s.role.toLowerCase()}`;
    set(this.clock, s.remainingMs === null ? "--:--" : clock(s.remainingMs));
    this.halves.forEach((e, i) => {
      // ● for a half played or being played, ○ for one to come.
      set(e, i + 1 <= Math.max(s.half, s.done) ? "●" : "○");
      e.className = i + 1 === s.half && s.done < s.half ? "half current" : "half";
    });
    set(this.vs, s.opponent === "" ? "" : `vs ${s.opponent}`);
    set(this.note, s.note);
  }
}

export function clock(ms: number): string {
  const total = Math.ceil(Math.max(0, ms) / 1000);
  const m = Math.floor(total / 60);
  const sec = total % 60;
  return `${String(m).padStart(2, "0")}:${String(sec).padStart(2, "0")}`;
}
