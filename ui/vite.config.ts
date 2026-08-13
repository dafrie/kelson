import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// kelson-server serves ConnectRPC on loopback with NO CORS handling and no
// authentication in v0 (ADR-0013 §3): it is a single-origin server that
// assumes the UI is served from the same origin it is. In production that is
// true — the UI ships as static assets behind the same listener. In
// development Vite serves on :5173, so this proxy is the whole story: it makes
// the browser's origin and the API's origin the same one, and no CORS
// middleware has to exist on the Go side to support it.
//
// Connect RPC paths are `/<package>.<Service>/<Method>`, so the single
// `/kelson.v1alpha1.` prefix covers SpecService, RenderService, ProfileService,
// DeployService and LogService at once — a new service needs no change here.
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
