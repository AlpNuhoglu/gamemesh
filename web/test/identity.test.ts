import { describe, expect, test } from "vitest";
import { Api, ApiError } from "../src/net/api";
import { EARLY_RENEWAL_MS, Identity, STORAGE_KEY, type StoredIdentity } from "../src/net/identity";
import { fakeFetch, MemoryKV, type Call } from "./fakes";

// The renewal table of section 3.6, one test per row, against a fake server.

const NOW = Date.parse("2026-11-22T12:00:00Z");
const iso = (ms: number) => new Date(ms).toISOString();

let guests = 0;
let logins = 0;

/** server is a fake of the two auth endpoints. login answers loginStatus. */
function server(loginStatus = 200, expiresIn = 2 * 3600_000) {
  guests = 0;
  logins = 0;
  return fakeFetch((c: Call) => {
    if (c.method === "POST" && c.url === "/api/v1/auth/guest") {
      guests++;
      return {
        status: 201,
        body: {
          token: `guest-token-${guests}`, expires_at: iso(NOW + expiresIn), secret: `secret-${guests}`,
          player: { id: `p-${guests}`, username: `Guest${guests}`, is_guest: true },
        },
      };
    }
    if (c.method === "POST" && c.url === "/api/v1/auth/login") {
      logins++;
      if (loginStatus !== 200) return { status: loginStatus, body: { error: "invalid credentials" } };
      const b = c.body as { identifier: string; password: string };
      return {
        status: 200,
        body: { token: `renewed-${logins}`, expires_at: iso(NOW + expiresIn), player: { id: "p-stored", username: b.identifier } },
      };
    }
    return undefined;
  });
}

function stored(expiresAt: number): StoredIdentity {
  return { playerId: "p-stored", username: "SwiftFox4821", token: "old-token", expiresAt: iso(expiresAt), secret: "the-secret" };
}

function kvWith(id: StoredIdentity): MemoryKV {
  const kv = new MemoryKV();
  kv.setItem(STORAGE_KEY, JSON.stringify(id));
  return kv;
}

function noSecretInUrls(calls: Call[], secret: string) {
  for (const c of calls) expect(c.url).not.toContain(secret);
}

describe("first visit", () => {
  test("one guest call, and the whole identity is kept", async () => {
    const s = server();
    const kv = new MemoryKV();
    const id = await new Identity(new Api(s.fetch), kv).ensure(NOW);
    expect(s.calls.map((c) => c.url)).toEqual(["/api/v1/auth/guest"]);
    expect(JSON.parse(kv.getItem(STORAGE_KEY)!)).toEqual({
      playerId: "p-1", username: "Guest1", token: "guest-token-1", expiresAt: iso(NOW + 2 * 3600_000), secret: "secret-1",
    });
    expect(id.playerId).toBe("p-1");
  });

  test("a returning player with time left makes no call", async () => {
    const s = server();
    const id = await new Identity(new Api(s.fetch), kvWith(stored(NOW + 3600_000))).ensure(NOW);
    expect(s.calls).toEqual([]);
    expect(id.token).toBe("old-token");
  });
});

describe("row 1: out of a match with under 10 minutes left", () => {
  test("renews with the username and the secret in the body", async () => {
    const s = server();
    const kv = kvWith(stored(NOW + 3600_000));
    const idn = new Identity(new Api(s.fetch), kv);
    await idn.ensure(NOW);
    const later = NOW + 3600_000 - EARLY_RENEWAL_MS + 1;
    expect(await idn.renewIfDue(later, false)).toBe(true);
    expect(s.calls).toHaveLength(1);
    expect(s.calls[0]!.body).toEqual({ identifier: "SwiftFox4821", password: "the-secret" });
    expect(JSON.parse(kv.getItem(STORAGE_KEY)!).token).toBe("renewed-1");
    // The secret is kept: the login answer does not carry it.
    expect(JSON.parse(kv.getItem(STORAGE_KEY)!).secret).toBe("the-secret");
    noSecretInUrls(s.calls, "the-secret");
  });

  test("not in a match", async () => {
    const s = server();
    const idn = new Identity(new Api(s.fetch), kvWith(stored(NOW + 3600_000)));
    await idn.ensure(NOW);
    expect(await idn.renewIfDue(NOW + 3600_000 - 60_000, true)).toBe(false);
    expect(s.calls).toEqual([]);
  });

  test("not with more than 10 minutes left", async () => {
    const s = server();
    const idn = new Identity(new Api(s.fetch), kvWith(stored(NOW + 3600_000)));
    await idn.ensure(NOW);
    expect(await idn.renewIfDue(NOW + 3600_000 - EARLY_RENEWAL_MS - 1, false)).toBe(false);
    expect(s.calls).toEqual([]);
  });

  test("a stored token already close to expiry is renewed at start-up", async () => {
    const s = server();
    const id = await new Identity(new Api(s.fetch), kvWith(stored(NOW + 60_000))).ensure(NOW);
    expect(s.calls.map((c) => c.url)).toEqual(["/api/v1/auth/login"]);
    expect(id.token).toBe("renewed-1");
  });
});

describe("row 2: a REST call answered 401", () => {
  test("renews and repeats the call once, with the new token", async () => {
    const s = server();
    const idn = new Identity(new Api(s.fetch), kvWith(stored(NOW + 3600_000)));
    await idn.ensure(NOW);
    const seen: string[] = [];
    const out = await idn.authed(async (token) => {
      seen.push(token);
      if (token === "old-token") throw new ApiError(401, "expired");
      return "ok";
    });
    expect(out).toBe("ok");
    expect(seen).toEqual(["old-token", "renewed-1"]);
    expect(logins).toBe(1);
  });

  test("a second 401 is not retried again", async () => {
    const s = server();
    const idn = new Identity(new Api(s.fetch), kvWith(stored(NOW + 3600_000)));
    await idn.ensure(NOW);
    let n = 0;
    await expect(
      idn.authed(async () => {
        n++;
        throw new ApiError(401, "expired");
      }),
    ).rejects.toThrow();
    expect(n).toBe(2);
    expect(logins).toBe(1);
  });

  test("another error is not a reason to renew", async () => {
    const s = server();
    const idn = new Identity(new Api(s.fetch), kvWith(stored(NOW + 3600_000)));
    await idn.ensure(NOW);
    await expect(idn.authed(async () => Promise.reject(new ApiError(500, "boom")))).rejects.toThrow();
    expect(logins).toBe(0);
  });
});

describe("row 4: the renewal itself answered 401", () => {
  test("the stored identity is dropped and a new guest made", async () => {
    const s = server(401);
    const kv = kvWith(stored(NOW + 3600_000));
    const idn = new Identity(new Api(s.fetch), kv);
    await idn.ensure(NOW);
    const id = await idn.renew();
    expect(s.calls.map((c) => c.url)).toEqual(["/api/v1/auth/login", "/api/v1/auth/guest"]);
    expect(id.playerId).toBe("p-1");
    expect(JSON.parse(kv.getItem(STORAGE_KEY)!).playerId).toBe("p-1");
    expect(JSON.parse(kv.getItem(STORAGE_KEY)!).secret).toBe("secret-1");
  });

  test("a renewal that fails otherwise keeps the identity", async () => {
    const s = server(503);
    const kv = kvWith(stored(NOW + 3600_000));
    const idn = new Identity(new Api(s.fetch), kv);
    await idn.ensure(NOW);
    await expect(idn.renew()).rejects.toThrow();
    expect(JSON.parse(kv.getItem(STORAGE_KEY)!).playerId).toBe("p-stored");
    expect(guests).toBe(0);
  });
});

test("concurrent renewals share one login", async () => {
  const s = server();
  const idn = new Identity(new Api(s.fetch), kvWith(stored(NOW + 3600_000)));
  await idn.ensure(NOW);
  const [a, b] = await Promise.all([idn.renew(), idn.renew()]);
  expect(a.token).toBe(b.token);
  expect(logins).toBe(1);
});
