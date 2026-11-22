import { defineConfig } from "vitest/config";

// The client is served by the gateway under /play/ (design document, section
// 9, note D), so every asset URL is built relative to that base.
//
// The build goes straight into the directory the gateway embeds. Only the
// app/ subdirectory is the build's: emptyOutDir clears it, and the tracked
// README next to it (which keeps the Go embed compiling on a checkout that
// has not built the client) is left alone.
//
// In development the Vite server proxies the API and the WebSocket to a
// local gateway, so the page and the API share an origin exactly as they do
// in production and nothing needs CORS.
const gateway = process.env.GATEWAY_URL ?? "http://127.0.0.1:8080";

export default defineConfig({
  base: "/play/",
  build: {
    outDir: "../internal/gateway/webui/embedded/app",
    emptyOutDir: true,
    assetsDir: "assets",
    sourcemap: false,
    target: "es2022",
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      "/api": { target: gateway, changeOrigin: false },
      "/ws": { target: gateway, ws: true, changeOrigin: false },
    },
  },
  test: {
    include: ["test/**/*.test.ts"],
    environment: "node",
  },
});
