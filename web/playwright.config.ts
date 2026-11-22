import { defineConfig } from "@playwright/test";

// The end-to-end runs drive the installed Google Chrome (no browser is
// downloaded). They need a local stack and are not part of npm test.
//
// By default they go through the Vite dev server, which proxies to the stack
// on 127.0.0.1:8080. With E2E_BASE_URL set they go to that address instead
// and start no dev server: E2E_BASE_URL=http://127.0.0.1:8080 runs them
// against the page as the gateway serves it, built and embedded, under its
// Content-Security-Policy.
const external = process.env.E2E_BASE_URL;

export default defineConfig({
  testDir: "e2e",
  ...(external ? {} : { globalSetup: "./e2e/warmup.ts" }),
  timeout: 240_000,
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: {
    channel: "chrome",
    headless: true,
    baseURL: external ?? "http://127.0.0.1:5173",
    viewport: { width: 1000, height: 760 },
  },
  ...(external
    ? {}
    : {
        webServer: {
          command: "npx vite --host 127.0.0.1",
          url: "http://127.0.0.1:5173/play/",
          reuseExistingServer: true,
          timeout: 30_000,
        },
      }),
});
