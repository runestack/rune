//go:build e2e
// +build e2e

// Package harness spins up a real runed server for end-to-end tests.
//
// Each call to New gives the test a fully isolated instance: its own
// temp data directory, its own generated runefile, dynamically
// allocated gRPC/HTTP ports, a bootstrapped admin token, and captured
// server logs (dumped automatically when the test fails). The runed
// and rune binaries are built once per `go test` process.
//
// Tests talk to the server three ways:
//
//   - ctx.CLI            — the real `rune` binary, pre-authenticated
//   - ctx.Conn()         — gRPC connection for generated service clients
//   - ctx.HTTPURL(path)  — base URL for the HTTP listener (/v1 transcoder, UI)
//
// The server always runs with --dev-mode so no root, nftables, or
// privileged ports are needed; the harness works on a laptop and on CI
// runners alike. Tests that need actual containers must call
// RequireDocker(t) and tolerate being skipped when no daemon is
// available — everything else is control-plane coverage that runs
// anywhere.
package harness
