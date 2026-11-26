import { expect, test, type Page } from "@playwright/test";
import { Mover, probe, shot, spread, waitState } from "./helpers";

// Two pages in two browser contexts, so two guest identities: both arrive at
// once, are matched with each other, both move, and the match reaches its
// end on both.
test("two players: matched with each other, both move, both see the end", async ({ browser }) => {
  const ctxA = await browser.newContext();
  const ctxB = await browser.newContext();
  const a = await ctxA.newPage();
  const b = await ctxB.newPage();
  const errors: string[] = [];
  for (const p of [a, b]) p.on("pageerror", (e) => errors.push(String(e)));

  await Promise.all([a.goto("/play/?debug=1"), b.goto("/play/?debug=1")]);
  await Promise.all([waitState(a, "COUNTDOWN_1", 30_000), waitState(b, "COUNTDOWN_1", 30_000)]);

  // The same room, and the opponent is the other page, not the bot.
  const roomA = await a.locator("body").getAttribute("data-room");
  const roomB = await b.locator("body").getAttribute("data-room");
  expect(roomA).not.toBeNull();
  expect(roomA).toBe(roomB);
  await expect(a.locator(".top .vs")).not.toHaveText("vs Bot");
  const roleA = await a.locator("body").getAttribute("data-role");
  const roleB = await b.locator("body").getAttribute("data-role");
  expect(new Set([roleA, roleB])).toEqual(new Set(["HUNTER", "RUNNER"]));
  await shot(a, `two-01-countdown-A-${roleA}`);
  await shot(b, `two-01-countdown-B-${roleB}`);

  const halves: [string, string][] = [["HALF_1_ACTIVE", "half1"], ["HALF_2_ACTIVE", "half2"]];
  for (const [st, name] of halves) {
    await Promise.all([waitState(a, st, 15_000), waitState(b, st, 15_000)]);
    const ma = new Mover(a, 0, 0.3);
    const mb = new Mover(b, Math.PI / 2, 0.38);
    // Pictures once the runner's trail has had time to appear (trail_ms).
    await a.waitForTimeout(3500);
    const ra = await a.locator("body").getAttribute("data-role");
    const rb = await b.locator("body").getAttribute("data-role");
    if ((await a.locator("body").getAttribute("data-state")) === st) {
      await Promise.all([shot(a, `two-02-${name}-A-${ra}`), shot(b, `two-02-${name}-B-${rb}`)]);
    }
    const next = st === "HALF_1_ACTIVE" ? "ROLE_SWAP" : "FINISHED";
    await Promise.all([waitState(a, next, 60_000), waitState(b, next, 60_000)]);
    await Promise.all([ma.stop(), mb.stop()]);
    if (next === "ROLE_SWAP") await Promise.all([shot(a, "two-03-roleswap-A"), shot(b, "two-03-roleswap-B")]);
  }

  for (const [p, tag] of [[a, "A"], [b, "B"]] as [Page, string][]) {
    await expect(p.locator('.screen[data-screen="end"]')).toBeVisible();
    await expect(p.getByRole("button", { name: "Play again" })).toBeVisible();
    await shot(p, `two-04-end-${tag}`);
    const pr = await probe(p);
    expect(spread(pr.sent)).toBeGreaterThan(50); // this page's character moved
    expect(pr.drawn.filter((d) => d.live === true).length).toBeGreaterThan(10); // and it saw the other move live as HUNTER
  }
  const titles = await Promise.all([a.locator(".screen h1").textContent(), b.locator(".screen h1").textContent()]);
  // The two pages agree on the result.
  expect(new Set(titles).size === 2 || titles.every((t) => t === "Draw")).toBe(true);
  expect(errors).toEqual([]);
  await ctxA.close();
  await ctxB.close();
});
