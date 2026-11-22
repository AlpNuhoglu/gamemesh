// Stand-ins for the browser and the server, shared by the tests.

import type { FetchFn } from "../src/net/api";
import type { KV } from "../src/net/identity";
import type { Timers, WSLike } from "../src/net/socket";

export interface Call {
  method: string;
  url: string;
  body: unknown;
  auth: string | undefined;
}

export type Route = (c: Call) => { status: number; body?: unknown } | undefined;

/** fakeFetch answers through route and records every call. */
export function fakeFetch(route: Route): { fetch: FetchFn; calls: Call[] } {
  const calls: Call[] = [];
  const fetch: FetchFn = async (url, init) => {
    const headers = (init?.headers ?? {}) as Record<string, string>;
    const c: Call = {
      method: init?.method ?? "GET",
      url,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      auth: headers["Authorization"],
    };
    calls.push(c);
    const a = route(c) ?? { status: 404, body: { error: "not found" } };
    return new Response(a.body === undefined ? "" : JSON.stringify(a.body), { status: a.status });
  };
  return { fetch, calls };
}

export class MemoryKV implements KV {
  m = new Map<string, string>();
  getItem(k: string): string | null {
    return this.m.get(k) ?? null;
  }
  setItem(k: string, v: string): void {
    this.m.set(k, v);
  }
  removeItem(k: string): void {
    this.m.delete(k);
  }
}

/** FakeWS records what is sent and lets the test open and close it. */
export class FakeWS implements WSLike {
  static all: FakeWS[] = [];
  readyState = 0;
  sent: string[] = [];
  closedWith: number | undefined;
  onopen: ((ev: unknown) => void) | null = null;
  onclose: ((ev: { code: number; reason: string }) => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;

  constructor(readonly url: string) {
    FakeWS.all.push(this);
  }
  send(data: string): void {
    this.sent.push(data);
  }
  close(code?: number): void {
    this.closedWith = code;
    this.readyState = 3;
  }
  // Test side.
  open(): void {
    this.readyState = 1;
    this.onopen?.({});
  }
  serverClose(code: number, reason = ""): void {
    this.readyState = 3;
    this.onclose?.({ code, reason });
  }
  receive(m: unknown): void {
    this.onmessage?.({ data: JSON.stringify(m) });
  }
}

/** ManualTimers runs timers only when the test says so. */
export class ManualTimers implements Timers {
  private next = 1;
  pending = new Map<number, { fn: () => void; ms: number; every: boolean }>();
  setTimeout(fn: () => void, ms: number): unknown {
    const id = this.next++;
    this.pending.set(id, { fn, ms, every: false });
    return id;
  }
  clearTimeout(id: unknown): void {
    this.pending.delete(id as number);
  }
  setInterval(fn: () => void, ms: number): unknown {
    const id = this.next++;
    this.pending.set(id, { fn, ms, every: true });
    return id;
  }
  clearInterval(id: unknown): void {
    this.pending.delete(id as number);
  }
  /** timeouts are the pending one-shot timers' delays. */
  timeouts(): number[] {
    return [...this.pending.values()].filter((t) => !t.every).map((t) => t.ms);
  }
  /** fireTimeouts runs every pending one-shot timer. */
  fireTimeouts(): void {
    for (const [id, t] of [...this.pending]) {
      if (t.every) continue;
      this.pending.delete(id);
      t.fn();
    }
  }
}

/** settle lets pending promise callbacks run. */
export async function settle(): Promise<void> {
  for (let i = 0; i < 10; i++) await Promise.resolve();
  await new Promise((r) => setTimeout(r, 0));
}
