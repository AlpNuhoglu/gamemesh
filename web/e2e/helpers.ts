import { expect, type Page } from "@playwright/test";

// Screenshots go to e2e-shots/ (ignored by git; Playwright empties
// test-results/ at every run).
export async function shot(page: Page, name: string): Promise<string> {
  const path = `e2e-shots/${name}.png`;
  // A capture has been seen to hang on the first picture of a run right
  // after the dev server reloaded the page; it is retried rather than
  // waited on for the whole test.
  for (let i = 0; ; i++) {
    try {
      await page.screenshot({ path, timeout: 10_000 });
      return path;
    } catch (e) {
      if (i >= 2) throw e;
    }
  }
}

/** waitState waits until the page's room is in state (the body's data-state). */
export async function waitState(page: Page, state: string, timeout = 60_000): Promise<void> {
  await expect(page.locator(`body[data-state="${state}"]`)).toBeAttached({ timeout });
}

/**
 * waitDrawnOrLeft waits until the opponent (or, as RUNNER, the hunter's
 * trail) has been drawn n times, or the page has left state.
 */
export async function waitDrawnOrLeft(page: Page, n: number, state: string, timeout = 60_000): Promise<void> {
  await page.waitForFunction(
    ([n, state]) =>
      (window as unknown as { ghosttagProbe: { drawn: unknown[] } }).ghosttagProbe.drawn.length > (n as number) ||
      document.body.dataset["state"] !== state,
    [n, state] as const,
    { timeout },
  );
}

export async function state(page: Page): Promise<string> {
  return (await page.locator("body").getAttribute("data-state")) ?? "";
}

/**
 * Mover drives the own character with real pointer events: the mouse
 * circles the arena's centre, so the character follows it around, until
 * stopped.
 */
export class Mover {
  private running = true;
  private done: Promise<void>;

  // speed: radians per 50 ms step, signed. 0.03 is 0.6 rad/s, slower than
  // a character can follow at full speed on that circle.
  constructor(page: Page, phase: number, radiusFrac = 0.33, speed = 0.03) {
    this.done = (async () => {
      const canvas = page.locator("canvas");
      let a = phase;
      while (this.running) {
        const box = await canvas.boundingBox().catch(() => null);
        if (box !== null) {
          const r = box.width * radiusFrac;
          await page.mouse
            .move(box.x + box.width / 2 + r * Math.cos(a), box.y + box.height / 2 + r * Math.sin(a))
            .catch(() => {});
        }
        a += speed;
        await page.waitForTimeout(50).catch(() => {});
      }
    })();
  }

  async stop(): Promise<void> {
    this.running = false;
    await this.done;
  }
}

export interface ProbeEntry {
  t: number;
  x: number;
  y: number;
  live?: boolean;
}

/** probe reads the developer-mode probe (the page is opened with ?debug=1). */
export async function probe(page: Page): Promise<{ input: ProbeEntry[]; sent: ProbeEntry[]; drawn: ProbeEntry[] }> {
  return page.evaluate(() => {
    const p = (window as unknown as { ghosttagProbe: { input: ProbeEntry[]; sent: ProbeEntry[]; drawn: ProbeEntry[] } }).ghosttagProbe;
    return { input: [...p.input], sent: [...p.sent], drawn: [...p.drawn] };
  });
}

/**
 * trailOnlyAsRunner: in a page that ran first, every drawing of the
 * opponent before the first live one was the trail (live false), and none
 * after it. As RUNNER the hunter is never drawn live.
 */
export function liveAfterTrail(e: ProbeEntry[]): boolean {
  let firstLive = -1;
  let lastTrail = -1;
  e.forEach((d, i) => {
    if (d.live === true && firstLive < 0) firstLive = i;
    if (d.live === false) lastTrail = i;
  });
  return firstLive < 0 || lastTrail < firstLive;
}

/** spread is how far apart the positions of entries are, at most. */
export function spread(e: ProbeEntry[]): number {
  if (e.length === 0) return 0;
  const xs = e.map((p) => p.x);
  const ys = e.map((p) => p.y);
  return Math.max(Math.max(...xs) - Math.min(...xs), Math.max(...ys) - Math.min(...ys));
}
