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

## Transport

The SPA talks to the server with [`@connectrpc/connect-web`](https://connectrpc.com/docs/web/getting-started)
pointed at `<origin>/grpc`. Generate the typed clients with:

```bash
npm i -D @bufbuild/protoc-gen-es @connectrpc/protoc-gen-connect-es
make proto-ts   # or: cd web && buf generate
```

This reads `../pkg/api/proto/*.proto` and emits `web/src/gen/`.

## Build

`make ui` builds the SPA into `pkg/api/server/uiassets/dist`, which `go:embed`
bakes into `runed`. In Phase 1 it is a no-op that just verifies the placeholder
bundle exists.
