import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// kelson-server serves ConnectRPC on loopback with NO CORS handling
// (ADR-0013 §3): it is a single-origin server that assumes the UI is served
// from the same origin it is. In production that is true — the UI ships as
// static assets behind the same listener. In development Vite serves on :5173,
// so this proxy is the whole story: it makes the browser's origin and the API's
// origin the same one, and no CORS middleware has to exist on the Go side to
// support it.
//
// It is also what makes the session cookie work in development. The cookie is
// `SameSite=Lax` (#84's interim cut, ../docs/server.md), so a cross-site call to
// :8420 would not carry it; through this proxy there is no cross-site call.
// http-proxy forwards request and response headers — Cookie and Set-Cookie
// included — unchanged, and nothing below rewrites them.
//
// Connect RPC paths are `/<package>.<Service>/<Method>`, so the single
// `/kelson.v1alpha1.` prefix covers SpecService, RenderService, ProfileService,
// DeployService and LogService at once — a new service needs no change here.
// `/auth/` covers the three session endpoints for the same reason.
//
// `/forge/` is not an RPC prefix at all: it is the GitHub App manifest flow and
// the webhook listener (ADR-0033 decision 2, ADR-0034 decision 1), which are
// plain handlers on kelson-server's mux because a webhook body's schema is
// GitHub's and the manifest flow is browser redirects. It is here because
// "Connect GitHub" on the connections page is an ordinary anchor to
// `/forge/github/manifest/start`, and without this entry that navigation
// reaches Vite's dev server and 404s instead of reaching kelson-server.
//
// Streaming (DeployService.Deploy, LogService.FollowLogs) requires the proxy to
// pass bytes through as they arrive. http-proxy streams by default; nothing
// below turns that off, and nothing here may start buffering responses
// (`selfHandleResponse`, response interceptors, compression) without breaking
// live logs.
const API_TARGET = "http://127.0.0.1:8420";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/kelson.v1alpha1.": { target: API_TARGET, changeOrigin: false },
      "/auth/": { target: API_TARGET, changeOrigin: false },
      "/forge/": { target: API_TARGET, changeOrigin: false },
      "/healthz": { target: API_TARGET, changeOrigin: false },
    },
  },
  test: {
    environment: "jsdom",
    globals: false,
    setupFiles: ["src/test/setup.ts"],
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    // Vitest stubs every CSS import to an empty module, `?raw` included, which
    // is right for the component tests — they assert structure, not paint. The
    // one exception is the token file: src/styles/tokens.test.ts reads it as
    // text to check that the dark and light palettes define the same names,
    // and a stub would make that guard pass vacuously.
    css: { include: [/tokens\.css/] },
  },
});
