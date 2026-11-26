// The WebSocket: the envelope, reconnecting, and ping/RTT (sections 3.6, 6;
// docs/api.md for the close codes).
//
// The socket keeps itself up. It remembers the room it is in and joins it
// again on every new connection, so a reconnect during a match lands back in
// the match (section 2: the room waits T_grace for the player).
//
//   1008 (token expired, session revoked)  renew with the secret, reconnect
//   1001 (server shutting down)            reconnect after a random delay
//   anything else                          reconnect with a short backoff
//
// None of this is shown to the player. Only when connecting keeps failing
// does the socket raise onTrouble, for a quiet "reconnecting" note.
//
// A browser cannot see the status of a refused handshake (a 401 for an
// expired token is a plain close), so two attempts in a row that never open
// renew the token before the next one.

import { parseServer, type ClientMsg, type ServerMsg } from "./protocol";

/** WSLike is the part of the browser's WebSocket the socket uses. */
export interface WSLike {
  readonly readyState: number;
  send(data: string): void;
  close(code?: number, reason?: string): void;
  onopen: ((ev: unknown) => void) | null;
  onclose: ((ev: { code: number; reason: string }) => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onerror: ((ev: unknown) => void) | null;
}

export type WSCtor = new (url: string) => WSLike;

export interface Timers {
  setTimeout(fn: () => void, ms: number): unknown;
  clearTimeout(id: unknown): void;
  setInterval(fn: () => void, ms: number): unknown;
  clearInterval(id: unknown): void;
}

export interface TokenSource {
  /** token is the token to connect with (making an identity if needed). */
  token(): Promise<string>;
  /** renew takes a new token. */
  renew(): Promise<unknown>;
}

export interface SocketDeps {
  WebSocket: WSCtor;
  url: (token: string) => string;
  identity: TokenSource;
  timers: Timers;
  random: () => number;
  now: () => number;
}

const OPEN = 1;

// Reconnect pacing. The room waits 5 s for a player who dropped (T_grace),
// so the first attempts are quick.
const BACKOFF_MIN_MS = 100;
const BACKOFF_MAX_MS = 2000;
// 1001: a random delay, so a rollout's clients do not all come back at once.
const SHUTDOWN_JITTER_MIN_MS = 250;
const SHUTDOWN_JITTER_SPAN_MS = 2000;
// After this many failed attempts in a row the player is told, quietly.
const TROUBLE_AFTER = 3;
// Ping every 2 s while in a room: section 6's 0.5 Hz.
const PING_INTERVAL_MS = 2000;

export class GameSocket {
  onMessage: (m: ServerMsg) => void = () => {};
  /** onOpen runs after every (re)connection, after the room is rejoined. */
  onOpen: () => void = () => {};
  /** onTrouble tells the page that connecting keeps failing, or no longer does. */
  onTrouble: (trouble: boolean) => void = () => {};

  /** rtt is the last ping's round trip in ms, or null. */
  rtt: number | null = null;
  framesIn = 0;
  framesOut = 0;

  private ws: WSLike | null = null;
  private room: string | null = null;
  private started = false;
  private failures = 0;
  private neverOpened = 0;
  private troubled = false;
  private retry: unknown = null;
  private ping: unknown = null;

  constructor(private readonly d: SocketDeps) {}

  get isOpen(): boolean {
    return this.ws !== null && this.ws.readyState === OPEN;
  }

  get currentRoom(): string | null {
    return this.room;
  }

  /** connect starts the socket; it then keeps itself connected. */
  connect(): void {
    this.started = true;
    void this.open();
  }

  /**
   * reopen replaces the connection with one on the current token, after an
   * early renewal (section 3.6, first row).
   */
  reopen(): void {
    const old = this.ws;
    this.ws = null;
    if (old !== null) {
      old.onclose = null;
      old.onmessage = null;
      old.close(1000, "reopen");
    }
    this.stopPing();
    void this.open();
  }

  /** join enters room, now and after every reconnect. */
  join(room: string): void {
    this.room = room;
    this.send({ action: "join", room });
    this.startPing();
  }

  /** leave leaves the room, if in one. */
  leave(): void {
    if (this.room !== null) this.send({ action: "leave", room: this.room });
    this.room = null;
    this.stopPing();
  }

  /** send writes one message; false if the socket is not open. */
  send(m: ClientMsg): boolean {
    if (!this.isOpen) return false;
    this.ws!.send(JSON.stringify(m));
    this.framesOut++;
    return true;
  }

  private async open(): Promise<void> {
    if (this.retry !== null) {
      this.d.timers.clearTimeout(this.retry);
      this.retry = null;
    }
    let token: string;
    try {
      token = await this.d.identity.token();
    } catch {
      this.scheduleRetry(this.backoff());
      return;
    }
    const ws = new this.d.WebSocket(this.d.url(token));
    this.ws = ws;
    let opened = false;
    ws.onopen = () => {
      if (this.ws !== ws) return;
      opened = true;
      this.failures = 0;
      this.neverOpened = 0;
      if (this.troubled) {
        this.troubled = false;
        this.onTrouble(false);
      }
      if (this.room !== null) {
        this.send({ action: "join", room: this.room });
        this.startPing();
      }
      this.onOpen();
    };
    ws.onmessage = (ev) => {
      if (this.ws !== ws || typeof ev.data !== "string") return;
      this.framesIn++;
      const m = parseServer(ev.data);
      if (m === null) return;
      if (m.type === "Game" && m.data.t === "pong") this.rtt = this.d.now() - m.data.cts;
      this.onMessage(m);
    };
    ws.onerror = () => {
      // A close always follows; it decides.
    };
    ws.onclose = (ev) => {
      if (this.ws !== ws) return;
      this.ws = null;
      this.stopPing();
      void this.closed(ev.code, opened);
    };
  }

  private async closed(code: number, opened: boolean): Promise<void> {
    if (!this.started) return;
    if (code === 1008) {
      // The token expired or the session was revoked: never reconnect with
      // it. A renewal that fails is retried like any failed attempt.
      try {
        await this.d.identity.renew();
      } catch {
        this.failed();
        this.scheduleRetry(this.backoff());
        return;
      }
      void this.open();
      return;
    }
    if (code === 1001) {
      this.scheduleRetry(SHUTDOWN_JITTER_MIN_MS + this.d.random() * SHUTDOWN_JITTER_SPAN_MS);
      return;
    }
    this.failed();
    if (!opened && ++this.neverOpened >= 2) {
      // Possibly a refused handshake: an expired or revoked token.
      this.neverOpened = 0;
      try {
        await this.d.identity.renew();
      } catch {
        // Retried below like any failure.
      }
    }
    this.scheduleRetry(this.backoff());
  }

  private failed(): void {
    this.failures++;
    if (this.failures >= TROUBLE_AFTER && !this.troubled) {
      this.troubled = true;
      this.onTrouble(true);
    }
  }

  private backoff(): number {
    return Math.min(BACKOFF_MAX_MS, BACKOFF_MIN_MS * 2 ** Math.max(0, this.failures - 1));
  }

  private scheduleRetry(ms: number): void {
    if (this.retry !== null) this.d.timers.clearTimeout(this.retry);
    this.retry = this.d.timers.setTimeout(() => {
      this.retry = null;
      void this.open();
    }, ms);
  }

  private startPing(): void {
    if (this.ping !== null || this.room === null) return;
    const tick = () => {
      if (this.room !== null) this.send({ action: "relay", room: this.room, data: { t: "ping", cts: this.d.now() } });
    };
    tick();
    this.ping = this.d.timers.setInterval(tick, PING_INTERVAL_MS);
  }

  private stopPing(): void {
    if (this.ping !== null) this.d.timers.clearInterval(this.ping);
    this.ping = null;
  }
}
