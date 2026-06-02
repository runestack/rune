package server

import (
	"embed"
	"io/fs"
)

// uiAssets holds the dashboard's built single-page-app bundle, embedded into
// the runed binary at compile time (RUNE-200).
//
// For Phase 1 this is a committed placeholder page ("UI not built"). The
// Phase 2 build pipeline (`make ui`) overwrites uiassets/dist with the real
// Vite output before `go build`, so upgrading the binary upgrades the UI with
// no separate deploy artifact. Backend-only contributors who never run
// `make ui` still compile against the placeholder, keeping `go build` green.
//
//go:embed all:uiassets/dist
var uiAssets embed.FS

// uiAssetsFS returns the embedded SPA filesystem rooted at the dist directory
// (so paths are "index.html", "assets/...", not "uiassets/dist/...").
func uiAssetsFS() (fs.FS, error) {
	return fs.Sub(uiAssets, "uiassets/dist")
}
