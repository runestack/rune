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
      // /v1 carries both HTTP (auth/refresh) and the exec WebSocket bridge, so
      // enable ws upgrade on this route. runed's WS bridge enforces same-origin
      // (coder/websocket rejects cross-origin), so rewrite the Origin header of
      // the upgraded request to the target — otherwise the browser's
      // localhost:5273 origin is rejected and the socket resets. Same-origin in
      // production (served under /ui), so this only matters for the dev proxy.
      "/v1": {
        target: "http://127.0.0.1:7861",
        changeOrigin: true,
        ws: true,
        configure: (proxy) => {
          proxy.on("proxyReqWs", (proxyReq) => {
            proxyReq.setHeader("origin", "http://127.0.0.1:7861");
          });
        },
      },
      "/healthz": { target: "http://127.0.0.1:7861", changeOrigin: true },
    },
  },
  build: {
    // Phase 2 build pipeline emits into the Go embed dir so `runed` picks it up.
    outDir: "../pkg/api/server/uiassets/dist",
    emptyOutDir: true,
  },
});
