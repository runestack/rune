# Rune Dashboard (web)

First-party web UI for Rune, embedded into the `runed` binary and served under
`/ui` (RUNE-200). See [`_docs/designs/RUNE-200-Dashboard-Embedded-UI.md`](../../_docs/designs/RUNE-200-Dashboard-Embedded-UI.md)
and the Phase 1 plan [`RUNE-200A`](../../_docs/designs/RUNE-200A-Dashboard-Phase1-Plumbing.md).

## Status: Phase 1 (plumbing)

The backend serving layer is in place:

- HTTP server on `:7861` (`runed --ui`, on by default).
- `/grpc/...` — Connect / gRPC-Web / JSON transcoder (`connectrpc.com/vanguard`)
  over the existing gRPC services, with the full auth/rbac interceptor chain.
- `/v1/ui/handoff/{code}` — CLI token handoff.
- `/ui/` — currently a placeholder page (`pkg/api/server/uiassets/dist`).

The SPA itself (Vite + React + TanStack Query + Tailwind) lands in **Phase 2**.

## Transport — validated ✅

The SPA talks to the server with [`@connectrpc/connect`](https://connectrpc.com/docs/web/getting-started)
pointed at `<origin>/grpc`. Generate the typed clients with:

```bash
cd web
npm install
npm run gen      # protoc + @bufbuild/@connectrpc plugins → src/gen/ (gitignored)
```

`npm run gen` reads `../pkg/api/proto/*.proto` via protoc. (`buf.gen.yaml` is kept
for buf ≥ 1.32 v2 config, but the protoc path is canonical because it works with
the protoc already used by `make proto`.)

### Streaming smoke (Phase-1 risk closed)

`scripts/smoke.ts` drives a live `runed` through the vanguard transcoder using the
exact Connect stack the SPA will use, proving both call shapes work end-to-end:

```bash
# 1. start a runed (no Docker needed for the API layer):
#    runed --dev-mode --node-role="" --ui --ui-require-tls=false
# 2. run the smoke:
cd web && RUNE_URL=http://127.0.0.1:7861/grpc npm run smoke
```

Expected output:

```
[smoke] UNARY AdminBootstrap OK — subject=…
[smoke] UNARY WhoAmI OK — subject=… policies=[root]
[smoke] STREAM frame 1: namespace=default event=1
[smoke] STREAM frame 2: namespace=system event=1
[smoke] SERVER-STREAMING OK — 2 frame(s) over the transcoder
[smoke] PASS ✅
```

**Known constraint:** unary and **server-streaming** (logs, watches) work over
Connect/HTTP1.1 — what the dashboard needs. True **client/bidi streaming** (exec)
cannot run in a browser over HTTP and will need a WebSocket bridge in Phase 4.

## Build

`make ui` builds the SPA into `pkg/api/server/uiassets/dist`, which `go:embed`
bakes into `runed`. In Phase 1 it is a no-op that just verifies the placeholder
bundle exists.
