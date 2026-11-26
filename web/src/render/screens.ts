// The state screens (section 3.7): looking for a match, the countdown, the
// role swap, the match end with "play again". No tutorial: the one sentence
// on the countdown and the role badge are the whole explanation.

import type { Role } from "../net/protocol";

export type ScreenName = "looking" | "countdown" | "swap" | "grace" | "end" | "none";

export class Screens {
  private readonly box: HTMLDivElement;
  private readonly title: HTMLHeadingElement;
  private readonly line: HTMLParagraphElement;
  private readonly big: HTMLDivElement;
  private readonly sub: HTMLParagraphElement;
  private readonly button: HTMLButtonElement;
  current: ScreenName = "none";
  onPlayAgain: () => void = () => {};

  constructor(private readonly root: HTMLElement) {
    this.box = document.createElement("div");
    this.box.className = "screen-box";
    this.title = document.createElement("h1");
    this.line = document.createElement("p");
    this.line.className = "line";
    this.big = document.createElement("div");
    this.big.className = "big";
    this.sub = document.createElement("p");
    this.sub.className = "sub";
    this.button = document.createElement("button");
    this.button.type = "button";
    this.button.textContent = "Play again";
    this.button.addEventListener("click", () => this.onPlayAgain());
    this.box.append(this.title, this.line, this.big, this.sub, this.button);
    root.append(this.box);
    this.hide();
  }

  private show(name: ScreenName, title: string, line: string, big: string, sub: string, button: boolean): void {
    this.current = name;
    this.root.dataset["screen"] = name;
    this.root.classList.toggle("passive", name === "grace");
    this.root.hidden = false;
    setText(this.title, title);
    setText(this.line, line);
    setText(this.big, big);
    setText(this.sub, sub);
    this.button.hidden = !button;
  }

  looking(detail: string): void {
    this.show("looking", "Looking for a match…", "", "", detail, false);
  }

  countdown(role: Role | "", sentence: string, remainingMs: number, opponent: string): void {
    this.show("countdown", `You are the ${role}`, sentence, seconds(remainingMs), opponent === "" ? "" : `vs ${opponent}`, false);
    this.root.dataset["role"] = role;
  }

  swap(role: Role | "", sentence: string, remainingMs: number, lastHalf: string): void {
    this.show("swap", `Roles swap: you are the ${role}`, sentence, seconds(remainingMs), lastHalf, false);
    this.root.dataset["role"] = role;
  }

  grace(text: string): void {
    this.show("grace", text, "", "", "", false);
  }

  end(title: string, line: string, sub: string): void {
    this.show("end", title, line, "", sub, true);
    this.button.focus();
  }

  hide(): void {
    this.current = "none";
    this.root.dataset["screen"] = "none";
    this.root.hidden = true;
  }
}

function setText(e: HTMLElement, t: string): void {
  if (e.textContent !== t) e.textContent = t;
  e.hidden = t === "";
}

function seconds(ms: number): string {
  return String(Math.max(1, Math.ceil(ms / 1000)));
}
