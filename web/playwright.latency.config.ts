import { defineConfig } from "@playwright/test";
import base from "./playwright.config";

// The browser latency measurement (docs/latency.md, "In a real browser").
// It is a measurement, not a test: it is run by hand with
//
//   E2E_BASE_URL=http://127.0.0.1:8080 npm run latency
//
// against a local stack whose gateway serves the built client, and writes
// its samples to bench-out/latency.json.
export default defineConfig({
  ...base,
  testDir: "bench",
  timeout: 900_000,
});
