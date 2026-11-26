// The room as the client knows it, from snap, half_end and match_end
// (sections 6 and 7). Every method that depends on time takes it.

import { arenaRadius } from "./arena";
import { isActive, type Cfg, type HalfEnd, type MatchEnd, type Role, type RoomState, type Snap, type SnapPlayer } from "../net/protocol";

export class RoomView {
  state: RoomState | null = null;
  cfg: Cfg | null = null;
  players: SnapPlayer[] = [];
  halves: HalfEnd[] = [];
  end: MatchEnd | null = null;
  /** The state's clock as the last snap gave it, and when it arrived. */
  private remMs = 0;
  private remAt = 0;

  constructor(readonly me: string) {}

  /** applySnap takes a snap received at now and returns the state before it. */
  applySnap(s: Snap, now: number): RoomState | null {
    const prev = this.state;
    this.state = s.state;
    this.cfg = s.cfg;
    this.players = s.players;
    this.remMs = s.rem_ms;
    this.remAt = now;
    return prev;
  }

  applyHalfEnd(h: HalfEnd): void {
    if (!this.halves.some((x) => x.half === h.half)) this.halves.push(h);
  }

  applyMatchEnd(m: MatchEnd, now: number): void {
    this.end = m;
    this.remMs = this.remaining(now);
    this.state = "FINISHED";
  }

  mine(): SnapPlayer | undefined {
    return this.players.find((p) => p.id === this.me);
  }

  opponent(): SnapPlayer | undefined {
    return this.players.find((p) => p.id !== this.me);
  }

  myRole(): Role | "" {
    return this.mine()?.role ?? "";
  }

  /** half is the half being played or next to be played: 1 or 2. */
  half(): 1 | 2 {
    switch (this.state) {
      case "HALF_2_ACTIVE":
      case "ROLE_SWAP":
        return 2;
      case "HALF_1_ACTIVE":
      case "WAITING":
      case "COUNTDOWN_1":
      case null:
        return 1;
    }
    return this.halves.length >= 1 ? 2 : 1;
  }

  /** remaining is what is left of the current state's clock at now, in ms. */
  remaining(now: number): number {
    switch (this.state) {
      case "DISCONNECT_GRACE":
      case "FINISHED":
      case null:
        return this.remMs; // frozen (section 2)
    }
    return Math.max(0, this.remMs - (now - this.remAt));
  }

  /** halfElapsed is the active time played in the current half at now. */
  halfElapsed(now: number): number {
    if (this.cfg === null) return 0;
    if (isActive(this.state) || this.state === "DISCONNECT_GRACE") {
      return Math.min(this.cfg.t_half_ms, Math.max(0, this.cfg.t_half_ms - this.remaining(now)));
    }
    return 0;
  }

  /** radius is R(t) at now. */
  radius(now: number): number {
    if (this.cfg === null) return 0;
    return arenaRadius(this.cfg, this.halfElapsed(now));
  }
}
