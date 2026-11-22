import { defineConfig } from "@playwright/test";

// The end-to-end runs drive the installed Google Chrome (no browser is
// downloaded) against the Vite dev server, which proxies to a local stack
// on 127.0.0.1:8080. They need that stack and are not part of npm test.
export default defineConfig({
  testDir: "e2e",
  globalSetup: "./e2e/warmup.ts",
  timeout: 240_000,
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: {
    channel: "chrome",
    headless: true,
    baseURL: "http://127.0.0.1:5173",
    viewport: { width: 1000, height: 760 },
  },
  webServer: {
    command: "npx vite --host 127.0.0.1",
    url: "http://127.0.0.1:5173/play/",
    reuseExistingServer: true,
    timeout: 30_000,
  },
});
