// The player's identity and its silent renewal (design document, section
// 3.6; the guest account is section 9, Note C).
//
// First visit: one POST /auth/guest, and {player id, username, token,
// expires_at, secret} is kept in localStorage. The token lives two hours;
// the client renews it with the secret through POST /auth/login, which is
// the renewal table of section 3.6:
//
//   out of a match, under 10 minutes left  renew (the caller reopens the socket)
//   a REST call answered 401              renew, repeat the call once
//   the WebSocket closed with 1008        renew (socket.ts reconnects and rejoins)
//   the renewal itself answered 401       drop the stored identity, new guest
//
// The player never sees any of it. The token and the secret are never
// logged, and the secret only ever travels in a POST body.

import { ApiError, type Api, type TokenAnswer } from "./api";

export interface StoredIdentity {
  playerId: string;
  username: string;
  token: string;
  expiresAt: string;
  secret: string;
}

/** KV is the part of localStorage identity uses. */
export interface KV {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

export const STORAGE_KEY = "ghosttag.identity";

/** How long before expiry the token is renewed out of a match (section 3.6). */
export const EARLY_RENEWAL_MS = 10 * 60 * 1000;

function parseStored(raw: string | null): StoredIdentity | null {
  if (raw === null) return null;
  try {
    const v = JSON.parse(raw) as Record<string, unknown>;
    const keys = ["playerId", "username", "token", "expiresAt", "secret"] as const;
    for (const k of keys) if (typeof v[k] !== "string" || v[k] === "") return null;
    return v as unknown as StoredIdentity;
  } catch {
    return null;
  }
}

export class Identity {
  private cur: StoredIdentity | null = null;
  private renewing: Promise<StoredIdentity> | null = null;
  private creating: Promise<StoredIdentity> | null = null;
  /** How many renewals ran; for the developer overlay. */
  renewals = 0;

  constructor(
    private readonly api: Api,
    private readonly store: KV,
  ) {}

  /** current is the identity in use, or null before ensure. */
  current(): StoredIdentity | null {
    return this.cur;
  }

  /**
   * ensure loads the stored identity, or makes a guest on a first visit. A
   * stored token that is due for renewal (or already expired) is renewed
   * here: at start-up the player is in no match.
   */
  async ensure(now: number): Promise<StoredIdentity> {
    if (this.cur === null) this.cur = parseStored(this.store.getItem(STORAGE_KEY));
    if (this.cur === null) return this.createGuest();
    if (this.dueForRenewal(now)) return this.renew();
    return this.cur;
  }

  /** dueForRenewal: under EARLY_RENEWAL_MS left on the token at now. */
  dueForRenewal(now: number): boolean {
    if (this.cur === null) return false;
    const exp = Date.parse(this.cur.expiresAt);
    return !Number.isFinite(exp) || exp - now < EARLY_RENEWAL_MS;
  }

  /**
   * renewIfDue is the first row of the table: out of a match and close to
   * expiry, renew. It says whether it renewed; the caller then reopens the
   * WebSocket with the new token.
   */
  async renewIfDue(now: number, inMatch: boolean): Promise<boolean> {
    if (inMatch || !this.dueForRenewal(now)) return false;
    await this.renew();
    return true;
  }

  /**
   * renew takes a new token with the secret. Concurrent callers share one
   * renewal. A 401 means the account is gone (deleted, or swept): the stored
   * identity is dropped and a new guest made.
   */
  renew(): Promise<StoredIdentity> {
    if (this.renewing !== null) return this.renewing;
    const cur = this.cur;
    if (cur === null) return this.createGuest();
    this.renewing = (async () => {
      try {
        const a = await this.api.login(cur.username, cur.secret);
        this.renewals++;
        return this.save(a, cur.secret);
      } catch (e) {
        if (e instanceof ApiError && e.status === 401) {
          this.drop();
          return this.createGuest();
        }
        throw e;
      } finally {
        this.renewing = null;
      }
    })();
    return this.renewing;
  }

  /**
   * authed runs a REST call with the token. On a 401 it renews and repeats
   * the call once; a second 401 is the caller's.
   */
  async authed<T>(call: (token: string) => Promise<T>): Promise<T> {
    const id = this.cur ?? (await this.createGuest());
    try {
      return await call(id.token);
    } catch (e) {
      if (!(e instanceof ApiError) || e.status !== 401) throw e;
    }
    const fresh = await this.renew();
    return call(fresh.token);
  }

  private createGuest(): Promise<StoredIdentity> {
    if (this.creating !== null) return this.creating;
    this.creating = (async () => {
      try {
        const g = await this.api.guest();
        return this.save(g, g.secret);
      } finally {
        this.creating = null;
      }
    })();
    return this.creating;
  }

  private save(a: TokenAnswer, secret: string): StoredIdentity {
    const id: StoredIdentity = {
      playerId: a.player.id,
      username: a.player.username,
      token: a.token,
      expiresAt: a.expires_at,
      secret,
    };
    this.cur = id;
    try {
      this.store.setItem(STORAGE_KEY, JSON.stringify(id));
    } catch {
      // Storage full or blocked (a private window): the identity lives for
      // this page only, and the next visit is a new guest.
    }
    return id;
  }

  private drop(): void {
    this.cur = null;
    try {
      this.store.removeItem(STORAGE_KEY);
    } catch {
      // As in save.
    }
  }
}
