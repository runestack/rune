import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The SPA is embedded into `runed` and served under /ui (RUNE-200), so the
// build uses a relative base. In dev, /grpc, /v1 and the exec WS are proxied
// to a local runed (default :7861) so the real API works without CORS.
export default defineConfig({
  base: "./",
  plugins: [react()],
  server: {
    port: 5273,
    proxy: {
      "/grpc": { target: "http://127.0.0.1:7861", changeOrigin: true },
      "/v1": { target: "http://127.0.0.1:7861", changeOrigin: true },
      "/healthz": { target: "http://127.0.0.1:7861", changeOrigin: true },
    },
  },
  build: {
    // Phase 2 build pipeline emits into the Go embed dir so `runed` picks it up.
    outDir: "../pkg/api/server/uiassets/dist",
    emptyOutDir: true,
  },
});
