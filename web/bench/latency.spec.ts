import { mkdirSync, writeFileSync } from "node:fs";
import { expect, test, type Page } from "@playwright/test";
import { waitState } from "../e2e/helpers";

// Real-browser, end-to-end latency: from a key going down in one player's
// page to the frame in which the other player's page draws the movement.
//
// Two pages (two browser contexts, so two guests) play each other. In every
// half the page that is watched is at rest; a key is pressed in it; the
// other page is asked for the first frame in which it drew the opponent at
// a new place. Both pages stamp with performance.timeOrigin +
// performance.now(), the same wall clock on one machine.
//
//   key      the keydown event's own timestamp, in the pressing page
//   applied  the frame that moved the pressing page's own character
//   sent     the first position packet that carries the movement
//   drawn    the frame in which the watching page drew it
//
// "live": the runner presses and the hunter watches (the hunter sees the
// runner live, through the interpolation buffer). "trail": the hunter
// presses and the runner watches (the runner sees the hunter only as the
// trail, trail_ms late by design).
//
// The timestamps of applied, sent and drawn come from the developer probe
// (?debug=1). "drawn" is the frame's requestAnimationFrame callback: the
// compositor and the display come after it and are not measured.

interface Entry {
  t: number;
  x: number;
  y: number;
  live?: boolean;
}

interface Sample {
  kind: "live" | "trail";
  half: number;
  /** Onset: key down to the first frame that drew the opponent elsewhere. */
  key: number;
  applied: number;
  sent: number;
  drawn: number | null;
  /**
   * Lag: for every frame that drew the opponent in motion, how long before
   * the opponent's own page had sent that position.
   */
  lags: number[];
}

const HOLD_MS = 200;

/** Presses left out because their stages were out of order. */
const skipped = { n: 0 };

async function install(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const w = window as unknown as { benchKeys: number[] };
    w.benchKeys = [];
    window.addEventListener("keydown", (e) => { if (!e.repeat) w.benchKeys.push(performance.timeOrigin + e.timeStamp); }, true);
  });
}

// The probe keeps a bounded number of entries, so everything is selected by
// time, never by index.
async function read(page: Page): Promise<{ now: number; keys: number[]; input: Entry[]; sent: Entry[]; drawn: Entry[] }> {
  return page.evaluate(() => {
    const w = window as unknown as { benchKeys: number[]; ghosttagProbe: { input: Entry[]; sent: Entry[]; drawn: Entry[] } };
    return {
      now: performance.timeOrigin + performance.now(),
      keys: [...w.benchKeys],
      input: [...w.ghosttagProbe.input],
      sent: [...w.ghosttagProbe.sent],
      drawn: [...w.ghosttagProbe.drawn],
    };
  });
}

const state = (p: Page) => p.locator("body").getAttribute("data-state");
const role = (p: Page) => p.locator("body").getAttribute("data-role");

/** frameMs samples the page's frame interval over one second. */
async function frameMs(page: Page): Promise<number> {
  return page.evaluate(
    () =>
      new Promise<number>((resolve) => {
        const ts: number[] = [];
        const step = (t: number) => {
          ts.push(t);
          if (t - ts[0]! < 1000) requestAnimationFrame(step);
          else resolve((ts[ts.length - 1]! - ts[0]!) / (ts.length - 1));
        };
        requestAnimationFrame(step);
      }),
  );
}

/**
 * lagsOf matches each drawn position to the moment the sender had sent it.
 * The sender's packets are 40 ms apart and the character moves in a straight
 * line between two of them, so a drawn position that lies on the segment
 * between two consecutive packets was the sender's position at the
 * interpolated time. Positions at rest match no moving segment and are
 * skipped. minLag and maxLag bound the search to one key press.
 */
function lagsOf(sent: Entry[], drawn: Entry[], minLag: number, maxLag: number): number[] {
  const out: number[] = [];
  for (const d of drawn) {
    for (let i = 1; i < sent.length; i++) {
      const s1 = sent[i - 1]!;
      const s2 = sent[i]!;
      const vx = s2.x - s1.x;
      const vy = s2.y - s1.y;
      const len2 = vx * vx + vy * vy;
      if (len2 < 1e-6) continue; // at rest
      const u = ((d.x - s1.x) * vx + (d.y - s1.y) * vy) / len2;
      if (u <= 0 || u > 1) continue;
      const off = Math.hypot(s1.x + u * vx - d.x, s1.y + u * vy - d.y);
      if (off > 0.05) continue;
      const lag = d.t - (s1.t + u * (s2.t - s1.t));
      if (lag >= minLag && lag <= maxLag) {
        out.push(lag);
        break;
      }
    }
  }
  return out;
}

/**
 * trials presses keys in presser while the room stays in halfState and
 * returns one sample per press. quietMs is how long the watcher must have
 * drawn nothing new before a press; waitMs how long is waited for the
 * movement to show; minLag and maxLag bound the lag that is looked for.
 */
async function trials(
  kind: "live" | "trail",
  half: number,
  halfState: string,
  presser: Page,
  watcher: Page,
  quietMs: number,
  waitMs: number,
  minLag: number,
  maxLag: number,
): Promise<Sample[]> {
  const out: Sample[] = [];
  const keys = ["KeyW", "KeyS"];
  for (let i = 0; (await state(presser)) === halfState; i++) {
    // The watched character must be at rest on the watcher's screen.
    let before = await read(watcher);
    for (let tries = 0; tries < 40; tries++) {
      const last = before.drawn[before.drawn.length - 1];
      if (last !== undefined && before.now - last.t >= quietMs) break;
      await watcher.waitForTimeout(100);
      before = await read(watcher);
    }
    const rest = before.drawn[before.drawn.length - 1];
    if (rest === undefined || (await state(presser)) !== halfState) break;
    const mark = (await read(presser)).now;

    await presser.keyboard.down(keys[i % 2]!);
    await presser.waitForTimeout(HOLD_MS);
    await presser.keyboard.up(keys[i % 2]!);
    await watcher.waitForTimeout(waitMs);
    if ((await state(presser)) !== halfState) break;

    const p = await read(presser);
    const w = await read(watcher);
    const key = p.keys.find((t) => t >= mark - 5);
    if (key === undefined) continue;
    const applied = p.input.find((e) => e.t >= key - 1);
    // The path the presser sent: from its last packet at rest onwards.
    const path = p.sent.filter((e) => e.t >= mark - 60);
    const start = path[0];
    const sent = start === undefined ? undefined : path.find((e) => e.x !== start.x || e.y !== start.y);
    if (applied === undefined || sent === undefined) continue;
    // A press is only a sample if its stages are in order. They are not
    // when the character was moved by something else in the same moment (a
    // clamp from the server shifts it without any input).
    if (sent.t < applied.t - 1) {
      skipped.n++;
      continue;
    }
    const seen = w.drawn.filter((e) => e.t > key && (kind === "live") === (e.live === true));
    const drawn = seen.find((e) => e.t >= sent.t && (e.x !== rest.x || e.y !== rest.y));
    out.push({ kind, half, key, applied: applied.t, sent: sent.t, drawn: drawn?.t ?? null, lags: lagsOf(path, seen, minLag, maxLag) });
  }
  return out;
}

function stats(v: number[]): { n: number; min: number; p50: number; p95: number; max: number; mean: number } {
  const s = [...v].sort((a, b) => a - b);
  const q = (p: number) => s[Math.min(s.length - 1, Math.floor(p * s.length))]!;
  const r = (x: number) => Math.round(x * 10) / 10;
  return { n: s.length, min: r(s[0]!), p50: r(q(0.5)), p95: r(q(0.95)), max: r(s[s.length - 1]!), mean: r(s.reduce((a, b) => a + b, 0) / s.length) };
}

test("browser latency: key press to the opponent's frame", async ({ browser }) => {
  const ctxA = await browser.newContext();
  const ctxB = await browser.newContext();
  const a = await ctxA.newPage();
  const b = await ctxB.newPage();
  await install(a);
  await install(b);
  await Promise.all([a.goto("/play/?debug=1"), b.goto("/play/?debug=1")]);

  const samples: Sample[] = [];
  const meta: Record<string, unknown> = { userAgent: await a.evaluate(() => navigator.userAgent), baseURL: test.info().project.use.baseURL };

  // Match 1 measures the live view, match 2 the trail.
  for (const kind of ["live", "trail"] as const) {
    await Promise.all([waitState(a, "HALF_1_ACTIVE", 120_000), waitState(b, "HALF_1_ACTIVE", 120_000)]);
    if (kind === "live") {
      meta["frameMsA"] = await frameMs(a);
      meta["frameMsB"] = await frameMs(b);
      // The numbers are only comparable at the display rate the document
      // states. A browser can run at half of it for reasons that have nothing
      // to do with the client: on 2026-11-26 every page, a blank one
      // included, ran at 33.3 ms a frame on a laptop at 19 % battery, and a
      // run taken then showed the first frame 10 ms later.
      for (const ms of [meta["frameMsA"], meta["frameMsB"]] as number[]) {
        expect(ms, "the browser is not drawing at 60 Hz, so this run cannot be compared with the published one (is the machine on low battery?)").toBeLessThan(20);
      }
      meta["cfg"] = await a.evaluate(() => (window as unknown as { ghosttagProbe: { cfg?: unknown } }).ghosttagProbe.cfg ?? null);
    }
    for (const [half, halfState] of [[1, "HALF_1_ACTIVE"], [2, "HALF_2_ACTIVE"]] as const) {
      await Promise.all([waitState(a, halfState, 120_000), waitState(b, halfState, 120_000)]);
      const runner = (await role(a)) === "RUNNER" ? a : b;
      const hunter = runner === a ? b : a;
      const got =
        kind === "live"
          ? await trials("live", half, halfState, runner, hunter, 300, 500, 0, 400)
          : await trials("trail", half, halfState, hunter, runner, 600, 2900, 1500, 2700);
      samples.push(...got);
    }
    await Promise.all([waitState(a, "FINISHED", 120_000), waitState(b, "FINISHED", 120_000)]);
    if (kind === "live") {
      // With the keyboard, not a click: once the pointer has moved, the
      // mouse steers the character whenever no key is down, and the pages
      // would no longer be at rest between presses.
      await Promise.all([a.getByRole("button", { name: "Play again" }).press("Enter"), b.getByRole("button", { name: "Play again" }).press("Enter")]);
    }
  }

  const summary: Record<string, unknown> = {};
  for (const kind of ["live", "trail"] as const) {
    const s = samples.filter((x) => x.kind === kind);
    if (s.length === 0) continue;
    const onset = s.filter((x) => x.drawn !== null);
    const lags = s.flatMap((x) => x.lags);
    summary[kind] = {
      presses: s.length,
      keyToApplied: stats(s.map((x) => x.applied - x.key)),
      appliedToSent: stats(s.map((x) => x.sent - x.applied)),
      ...(kind === "live" && onset.length > 0
        ? { sentToFirstDrawn: stats(onset.map((x) => x.drawn! - x.sent)), keyToFirstDrawn: stats(onset.map((x) => x.drawn! - x.key)) }
        : {}),
      ...(lags.length > 0 ? { lagBehindSender: stats(lags) } : {}),
    };
  }
  meta["pressesSkipped"] = skipped.n;
  mkdirSync("bench-out", { recursive: true });
  writeFileSync("bench-out/latency.json", JSON.stringify({ meta, summary, samples }, null, 2));
  console.log(JSON.stringify({ meta, summary }, null, 2));
  expect(samples.filter((x) => x.kind === "live").length).toBeGreaterThan(20);
});
