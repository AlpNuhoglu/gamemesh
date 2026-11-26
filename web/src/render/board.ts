// The leaderboard summary beside or below the arena: the top rows of the
// Ghost Tag all-time board.

import type { BoardRow } from "../net/api";

export class Board {
  private readonly list: HTMLOListElement;
  private readonly empty: HTMLParagraphElement;

  constructor(root: HTMLElement) {
    const h = document.createElement("h2");
    h.textContent = "Top players";
    this.list = document.createElement("ol");
    this.empty = document.createElement("p");
    this.empty.className = "empty";
    this.empty.textContent = "No ranked matches yet.";
    root.append(h, this.list, this.empty);
  }

  render(rows: BoardRow[], me: string): void {
    this.list.replaceChildren(
      ...rows.map((r) => {
        const li = document.createElement("li");
        const name = document.createElement("span");
        name.className = "name";
        name.textContent = `${r.rank}. ${r.name}`;
        const score = document.createElement("span");
        score.className = "score";
        score.textContent = String(r.score);
        if (r.name === me) li.className = "me";
        li.append(name, score);
        return li;
      }),
    );
    this.empty.hidden = rows.length > 0;
  }
}
