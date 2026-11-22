// The way into a match (section 3.7): the WebSocket is open, POST /queue
// with mode ghosttag, wait for MatchFound, join. After five seconds without
// an opponent the server gives a bot room, which arrives the same way.
//
// MatchFound is delivered at most once (section 11): if the socket was down
// when it fired, the room is cancelled after T_join and the client must
// queue again by itself. So whenever the socket comes back while the client
// waits, and every few seconds as a safety net, it asks GET /queue/status;
// a ticket that is gone without a MatchFound having arrived is queued again.

import type { Api } from "./api";
import type { Identity } from "./identity";
import type { ServerMsg } from "./protocol";
import type { GameSocket, Timers } from "./socket";

export type MatchmakingState = "idle" | "waiting-socket" | "queuing" | "queued" | "matched";

// Client pacing, not game rules.
const WATCHDOG_MS = 6000; // how often a waiting client checks its ticket
const RECHECK_MS = 1500; // a gone ticket's MatchFound may still be in flight
const RETRY_MS = 1000; // a failed queue call is retried after this

export class Matchmaker {
  state: MatchmakingState = "idle";
  /** onMatch: MatchFound arrived and the join was sent. */
  onMatch: (room: string, players: string[]) => void = () => {};
  /** requeues counts tickets queued again; for the developer overlay. */
  requeues = 0;
  private watchdog: unknown = null;
  private epoch = 0;

  constructor(
    private readonly api: Api,
    private readonly identity: Identity,
    private readonly socket: GameSocket,
    private readonly timers: Timers,
  ) {}

  /** start looks for a match. */
  start(): void {
    this.epoch++;
    this.stopWatchdog();
    if (!this.socket.isOpen) {
      this.state = "waiting-socket";
      return; // onSocketOpen queues
    }
    void this.queue(this.epoch);
  }

  /** stop gives up looking (the page is in a match, or leaving). */
  stop(): void {
    this.epoch++;
    this.state = "idle";
    this.stopWatchdog();
  }

  /** onSocketOpen is called after every (re)connection. */
  onSocketOpen(): void {
    if (this.state === "waiting-socket") void this.queue(this.epoch);
    else if (this.state === "queued") void this.check(this.epoch);
  }

  /** handle takes the hub messages that belong to matchmaking; true if it did. */
  handle(m: ServerMsg): boolean {
    if (m.type === "MatchFound") {
      if (this.state !== "queued" && this.state !== "queuing") return true; // not looking: stale
      this.epoch++;
      this.stopWatchdog();
      this.state = "matched";
      this.socket.join(m.room);
      this.onMatch(m.room, m.players);
      return true;
    }
    if (m.type === "JoinRejected" && this.state === "matched" && m.room === this.socket.currentRoom) {
      // The room is gone (cancelled, or too old): look again.
      this.socket.leave();
      this.requeues++;
      this.start();
      return true;
    }
    return false;
  }

  private async queue(epoch: number): Promise<void> {
    this.state = "queuing";
    try {
      await this.identity.authed((t) => this.api.queue(t));
    } catch {
      if (epoch === this.epoch) this.timers.setTimeout(() => epoch === this.epoch && void this.queue(epoch), RETRY_MS);
      return;
    }
    if (epoch !== this.epoch || this.state !== "queuing") return;
    this.state = "queued";
    this.startWatchdog(epoch);
  }

  private async check(epoch: number): Promise<void> {
    let queued: boolean;
    try {
      queued = await this.identity.authed((t) => this.api.queueStatus(t));
    } catch {
      return; // the watchdog asks again
    }
    if (queued || epoch !== this.epoch) return;
    // Gone without a MatchFound so far. Give one in flight a moment.
    this.timers.setTimeout(() => {
      if (epoch !== this.epoch || this.state !== "queued") return;
      this.requeues++;
      this.stopWatchdog();
      void this.queue(epoch);
    }, RECHECK_MS);
  }

  private startWatchdog(epoch: number): void {
    this.stopWatchdog();
    this.watchdog = this.timers.setInterval(() => {
      if (epoch === this.epoch && this.state === "queued") void this.check(epoch);
    }, WATCHDOG_MS);
  }

  private stopWatchdog(): void {
    if (this.watchdog !== null) this.timers.clearInterval(this.watchdog);
    this.watchdog = null;
  }
}
