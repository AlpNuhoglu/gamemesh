// The REST calls the client makes (docs/api.md): a guest account, its
// renewal through login, the Ghost Tag queue, and the Ghost Tag board.
//
// Every call goes to the page's own origin: in production the gateway serves
// the page, in development Vite proxies /api to it. fetch is a parameter so
// the tests can stand in for the server.

export type FetchFn = (input: string, init?: RequestInit) => Promise<Response>;

/** ApiError is a non-2xx answer; status 0 is a network failure. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export interface PlayerRef {
  id: string;
  username: string;
}

export interface TokenAnswer {
  token: string;
  expires_at: string;
  player: PlayerRef;
}

export interface GuestAnswer extends TokenAnswer {
  secret: string;
}

export interface BoardRow {
  name: string;
  score: number;
  rank: number;
}

// The rank a Ghost Tag ticket is queued with. Matchmaking pairs tickets
// within a rank window; the client has no rating of its own, and every guest
// starts at the same rank (the brief of the client step gives this body).
const QUEUE_BODY = { rank: 1000, mode: "ghosttag" } as const;

function isTokenAnswer(v: unknown): v is TokenAnswer {
  if (typeof v !== "object" || v === null) return false;
  const o = v as Record<string, unknown>;
  const p = o["player"] as Record<string, unknown> | undefined;
  return typeof o["token"] === "string" && typeof o["expires_at"] === "string" &&
    typeof p === "object" && p !== null && typeof p["id"] === "string" && typeof p["username"] === "string";
}

export class Api {
  constructor(
    private readonly fetchFn: FetchFn,
    private readonly base = "",
  ) {}

  private async call(method: string, path: string, opts: { token?: string; body?: unknown } = {}): Promise<unknown> {
    const headers: Record<string, string> = {};
    if (opts.body !== undefined) headers["Content-Type"] = "application/json";
    if (opts.token !== undefined) headers["Authorization"] = `Bearer ${opts.token}`;
    const init: RequestInit = { method, headers };
    if (opts.body !== undefined) init.body = JSON.stringify(opts.body);
    let res: Response;
    try {
      res = await this.fetchFn(this.base + path, init);
    } catch {
      throw new ApiError(0, `${method} ${path}: network error`);
    }
    if (!res.ok) {
      // The error text is the server's {"error": ...}; never the request.
      throw new ApiError(res.status, `${method} ${path}: ${res.status}`);
    }
    const text = await res.text();
    if (text === "") return null;
    try {
      return JSON.parse(text);
    } catch {
      throw new ApiError(res.status, `${method} ${path}: not JSON`);
    }
  }

  /** guest makes a guest account: POST /api/v1/auth/guest, no body. */
  async guest(): Promise<GuestAnswer> {
    const v = await this.call("POST", "/api/v1/auth/guest");
    if (!isTokenAnswer(v) || typeof (v as unknown as Record<string, unknown>)["secret"] !== "string") {
      throw new ApiError(500, "guest: unexpected answer");
    }
    return v as GuestAnswer;
  }

  /** login renews a guest's token with its username and secret, in the body. */
  async login(identifier: string, password: string): Promise<TokenAnswer> {
    const v = await this.call("POST", "/api/v1/auth/login", { body: { identifier, password } });
    if (!isTokenAnswer(v)) throw new ApiError(500, "login: unexpected answer");
    return v;
  }

  /** queue puts the player in the Ghost Tag queue. */
  async queue(token: string): Promise<void> {
    await this.call("POST", "/api/v1/queue", { token, body: QUEUE_BODY });
  }

  /** queueStatus says whether the player is still in a queue. */
  async queueStatus(token: string): Promise<boolean> {
    const v = (await this.call("GET", "/api/v1/queue/status", { token })) as Record<string, unknown> | null;
    return v !== null && v["queued"] === true;
  }

  /** board is the top of the Ghost Tag all-time board (public). */
  async board(limit: number): Promise<BoardRow[]> {
    const v = (await this.call("GET", `/api/v1/leaderboard/ghosttag?board=alltime&limit=${limit}`)) as
      | Record<string, unknown>
      | null;
    const rows = v?.["entries"];
    if (!Array.isArray(rows)) return [];
    return rows.filter(
      (r): r is BoardRow => typeof r?.name === "string" && typeof r?.score === "number" && typeof r?.rank === "number",
    );
  }
}
