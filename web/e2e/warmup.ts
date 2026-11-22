import { chromium } from "@playwright/test";

// Global setup: open the page once before the runs, with the API and the
// WebSocket blocked so no account is made and nothing is queued.
//
// After a source file changes, the Vite dev server makes the first page
// that loads reload itself shortly after; a screenshot taken across that
// reload hangs. The warm-up page takes that reload, so the runs do not.
export default async function warmup(): Promise<void> {
  const browser = await chromium.launch({ channel: "chrome", headless: true });
  try {
    const page = await browser.newPage();
    await page.route(/\/(api|ws)(\/|\?|$)/, (r) => r.abort());
    await page.goto("http://127.0.0.1:5173/play/");
    for (let i = 0; i < 3; i++) {
      await page.waitForTimeout(1000);
      if (await page.screenshot({ timeout: 5000 }).then(() => true, () => false)) break;
    }
  } finally {
    await browser.close();
  }
}
