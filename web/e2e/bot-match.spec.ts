import { expect, test } from "@playwright/test";
import { liveAfterTrail, Mover, probe, shot, spread, state, waitDrawnOrLeft, waitState } from "./helpers";

// One page: arrive, be matched with the bot (no human opponent within five
// seconds), play both halves with real pointer events, see the end screen.
// Every wait is on a state the page reports, never on a timer.
test("bot match: arrive, play both halves, see the end", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(String(e)));

  await page.goto("/play/?debug=1");
  // Arriving starts matchmaking, without a click.
  await expect(page.locator('.screen[data-screen="looking"]')).toBeVisible();
  await shot(page, "bot-01-looking");

  await waitState(page, "COUNTDOWN_1", 30_000);
  await expect(page.locator(".screen h1")).toHaveText("You are the RUNNER"); // the bot hunts first
  await expect(page.locator(".top .vs")).toHaveText("vs Bot");
  await shot(page, "bot-02-countdown");

  await waitState(page, "HALF_1_ACTIVE", 10_000);
  // Straight away from the hunter: the pointer rests at the far edge on
  // the runner's side.
  const m1 = new Mover(page, 0, 0.47, 0);
  // As RUNNER the hunter is drawn only as its trail, from trail_ms ago; the
  // picture is taken once the trail has started to move, if the half lasts
  // that long.
  await waitDrawnOrLeft(page, 3, "HALF_1_ACTIVE");
  if ((await state(page)) === "HALF_1_ACTIVE") await shot(page, "bot-03-half1-runner");
  await waitState(page, "ROLE_SWAP", 60_000);
  await m1.stop();
  await expect(page.locator(".screen h1")).toHaveText("Roles swap: you are the HUNTER");
  await shot(page, "bot-04-roleswap");

  const p1 = await probe(page);
  expect(liveAfterTrail(p1.drawn)).toBe(true); // as RUNNER the hunter was never drawn live
  expect(spread(p1.sent)).toBeGreaterThan(50); // the character moved

  await waitState(page, "HALF_2_ACTIVE", 10_000);
  const m2 = new Mover(page, Math.PI);
  await waitDrawnOrLeft(page, p1.drawn.length + 60, "HALF_2_ACTIVE");
  if ((await state(page)) === "HALF_2_ACTIVE") await shot(page, "bot-05-half2-hunter");
  await waitState(page, "FINISHED", 60_000);
  await m2.stop();

  await expect(page.locator('.screen[data-screen="end"]')).toBeVisible();
  await expect(page.getByRole("button", { name: "Play again" })).toBeVisible();
  await expect(page.locator(".screen .line")).toHaveText("Unranked match: no points.");
  await shot(page, "bot-06-end");

  const p2 = await probe(page);
  expect(p2.drawn.some((d) => d.live === true)).toBe(true); // the runner drawn live as HUNTER
  expect(errors).toEqual([]);

  // Play again is the same flow from the queue.
  await page.getByRole("button", { name: "Play again" }).click();
  await waitState(page, "COUNTDOWN_1", 30_000);
  await shot(page, "bot-07-again");
});
