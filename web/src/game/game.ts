// One match as the client plays it: the room state, the own character's
// prediction, the opponent's interpolation, the runner's trail, and the
// position packets. Time is always a parameter (section 3.5); the loop and
// the socket handler pass it in.

import { InterpBuffer, type InterpMode } from "./interp";
import { Predictor, type Input, type Vec } from "./predict";
import { RoomView } from "./state";
import { Trail, type TailPoint } from "./trail";
import { isActive, type Cfg, type GameMsg, type PosOut, type Role, type RoomState } from "../net/protocol";

export type GameEvent =
  | { kind: "state"; from: RoomState | null; to: RoomState }
  | { kind: "half_end" }
  | { kind: "match_end" }
  | { kind: "clamp" };

export interface Body extends Vec {
  role: Role | "";
  name: string;
}

export interface View {
  state: RoomState | null;
  cfg: Cfg;
  radius: number;
  me: Body & { theta: number };
  /** The opponent drawn live (as HUNTER, or outside a half), or null. */
  opponent: (Body & { mode: InterpMode | "snap" }) | null;
  /** As RUNNER in a half: the hunter's trail, head first. Empty otherwise. */
  trail: TailPoint[];
  /** Where the hunter is drawn, for the capture ring; null when not drawn. */
  hunterAt: Vec | null;
}

export class Game {
  readonly room: RoomView;
  readonly pred = new Predictor();
  readonly interp = new InterpBuffer();
  readonly trail = new Trail(0);
  /** min(local receive time − sts): maps sts onto the local clock. */
  private stsOffset: number | null = null;
  private seq = 0;
  private lastSent: { x: number; y: number; t: number } | null = null;
  posIn = 0;
  posOut = 0;

  constructor(
    readonly me: string,
    readonly roomId: string,
  ) {
    this.room = new RoomView(me);
  }

  get cfg(): Cfg | null {
    return this.room.cfg;
  }

  /** onMessage applies one room message received at now. */
  onMessage(m: GameMsg, now: number): GameEvent | null {
    switch (m.t) {
      case "snap": {
        const from = this.room.applySnap(m, now);
        this.trail.setDelay(m.cfg.trail_ms);
        const mine = this.room.mine();
        if (mine !== undefined) {
          const resumed = from === "DISCONNECT_GRACE" && isActive(m.state);
          if (!isActive(m.state) && m.state !== "DISCONNECT_GRACE") {
            // Outside a half the character stands where the room put it.
            this.pred.reset(mine.x, mine.y, mine.theta);
          } else if (!isActive(from) && !resumed && from !== "DISCONNECT_GRACE") {
            // A half starts: on the spawn point.
            this.pred.reset(mine.x, mine.y, mine.theta);
          } else if (resumed) {
            // Back from DISCONNECT_GRACE: the server anchors the player
            // where it last validated them; glide there if apart.
            if (mine.x !== this.pred.x || mine.y !== this.pred.y) this.pred.correct(mine.x, mine.y, mine.theta, now);
          }
        }
        if (from !== m.state && (m.state === "COUNTDOWN_1" || m.state === "ROLE_SWAP")) {
          // A new half: the rings are cleared on the server, and here.
          this.interp.clear();
          this.trail.clear();
          this.lastSent = null;
        }
        const opp = this.room.opponent();
        if (opp !== undefined && opp.role === "HUNTER" && !isActive(from) && m.state !== "DISCONNECT_GRACE") {
          // Before a half and at its start the hunter stands on its spawn
          // point, and the trail says so: trail_ms into the half the runner
          // sees it there, where it really was trail_ms ago.
          this.trail.push(now, opp.x, opp.y);
        }
        return from === m.state ? null : { kind: "state", from, to: m.state };
      }
      case "pos": {
        const opp = this.room.opponent();
        if (opp === undefined || m.from !== opp.id) return null;
        this.posIn++;
        const off = now - m.sts;
        if (this.stsOffset === null || off < this.stsOffset) this.stsOffset = off;
        this.interp.push({ t: m.sts, x: m.x, y: m.y, vx: m.vx, vy: m.vy });
        if (this.room.cfg !== null) this.interp.prune(this.localToSts(now), this.room.cfg);
        // The trail is kept by local receive time (section 3.3).
        this.trail.push(now, m.x, m.y);
        return null;
      }
      case "clamp":
        this.pred.correct(m.x, m.y, m.theta, now);
        return { kind: "clamp" };
      case "half_end":
        this.room.applyHalfEnd(m);
        return { kind: "half_end" };
      case "match_end":
        this.room.applyMatchEnd(m, now);
        return { kind: "match_end" };
      case "pong":
        return null;
    }
  }

  /** canMove: a half is on and the own role is known. */
  canMove(): boolean {
    return isActive(this.room.state) && this.room.myRole() !== "" && this.room.cfg !== null;
  }

  /**
   * frame moves the own character for dtMs of input at now; true if it
   * moved. leadMs is how much later the server will validate this position
   * (up to one send interval, plus the one-way trip): while the arena
   * shrinks, the character is kept inside the radius of that moment, so a
   * player at the edge is not clamped by the shrink that happened in flight.
   */
  frame(now: number, dtMs: number, input: Input, leadMs = 0): boolean {
    const cfg = this.room.cfg;
    const role = this.room.myRole();
    if (cfg === null || role === "" || !isActive(this.room.state)) return false;
    return this.pred.step(cfg, input, dtMs, role, this.room.radius(now + leadMs));
  }

  /** packet is the position report to send at now, or null outside a half. */
  packet(now: number): PosOut | null {
    if (!this.canMove()) return null;
    const x = this.pred.x;
    const y = this.pred.y;
    let vx = 0;
    let vy = 0;
    if (this.lastSent !== null && now > this.lastSent.t) {
      const dt = (now - this.lastSent.t) / 1000;
      vx = (x - this.lastSent.x) / dt;
      vy = (y - this.lastSent.y) / dt;
    }
    this.lastSent = { x, y, t: now };
    this.seq++;
    this.posOut++;
    // x and y go at full precision: rounded, a position on the arena's edge
    // can land just outside it and be clamped. vx, vy are only a hint.
    return { t: "pos", seq: this.seq, cts: Math.round(now), x, y, vx: round2(vx), vy: round2(vy) };
  }

  /** view is what to draw at now; null before the first snap. */
  view(now: number): View | null {
    const cfg = this.room.cfg;
    const mine = this.room.mine();
    if (cfg === null || mine === undefined) return null;
    const drawn = this.pred.drawn(now);
    const me = { x: drawn.x, y: drawn.y, theta: this.pred.theta, role: mine.role, name: mine.name };
    const oppSnap = this.room.opponent();
    let opponent: View["opponent"] = null;
    let trail: TailPoint[] = [];
    let hunterAt: Vec | null = mine.role === "HUNTER" ? me : null;
    if (oppSnap !== undefined) {
      const live = isActive(this.room.state) || this.room.state === "DISCONNECT_GRACE";
      if (mine.role === "RUNNER") {
        // The hunter is never drawn live, not even standing on its spawn
        // point before a half: only its trail, in a half (section 3.3).
        if (live) {
          this.trail.forget(now);
          trail = this.trail.tail(now);
          hunterAt = trail[0] ?? null;
        }
      } else if ((live || this.room.state === "FINISHED") && this.interp.size > 0) {
        // In a half; and after the match, where it last was.
        this.interp.prune(this.localToSts(now), cfg);
        const d = this.interp.sample(this.localToSts(now), cfg);
        opponent = { x: d.x, y: d.y, role: oppSnap.role, name: oppSnap.name, mode: d.mode };
      } else {
        opponent = { x: oppSnap.x, y: oppSnap.y, role: oppSnap.role, name: oppSnap.name, mode: "snap" };
      }
      if (opponent !== null && oppSnap.role === "HUNTER") hunterAt = opponent;
    }
    return { state: this.room.state, cfg, radius: this.room.radius(now), me, opponent, trail, hunterAt };
  }

  /** localToSts maps local time onto the room's sts clock. */
  private localToSts(now: number): number {
    return this.stsOffset === null ? now : now - this.stsOffset;
  }

  /** interpState describes the interpolation buffer, for the overlay. */
  interpState(now: number): string {
    const n = this.interp.newest;
    if (n === undefined || this.cfg === null) return "empty";
    const age = this.localToSts(now) - n.t;
    const mode = this.interp.sample(this.localToSts(now), this.cfg).mode;
    return `${this.interp.size} samples, newest ${age.toFixed(0)} ms old, ${mode}`;
  }
}

function round2(f: number): number {
  return Math.round(f * 100) / 100;
}
