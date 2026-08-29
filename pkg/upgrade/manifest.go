package upgrade

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Staging layout under <data-dir>/upgrade/. The directory is owned by the
// unprivileged service user; everything in it is untrusted input to the
// root applier, which is why the applier copies files out via validated
// fds before believing anything (see applier.go).
const (
	stagingDirName = "upgrade"
	readyName      = "ready"
	manifestName   = "manifest.json"
	tarballName    = "server.tar.gz"

	// Workdir is the root-owned applier workspace. 0755 root:root: the
	// service user cannot create or rename inside it, which is what makes
	// the result file unforgeable, and world-readable is fine — the
	// contents are public release binaries and the (public) result.
	// It lives on tmpfs, so artifacts are deleted as soon as each step is
	// done with them to bound RAM use.
	Workdir    = "/run/runed-upgrade"
	ResultName = "result.json"

	// manifestMaxAge: an applier finding a manifest staged longer ago
	// than this treats it as stale and consumes it as a no-op — a crash
	// mid-staging must not replay a weeks-old upgrade at next boot.
	manifestMaxAge = 15 * time.Minute
)

// StagingDir returns <data-dir>/upgrade.
func StagingDir(dataDir string) string { return filepath.Join(dataDir, stagingDirName) }

// ReadyPath is the file whose creation triggers the applier's path unit.
func ReadyPath(dataDir string) string { return filepath.Join(StagingDir(dataDir), readyName) }

// Manifest describes one staged upgrade. The requester's identity is
// deliberately NOT here: the manifest is writable by the service user, so
// identity is taken from the authenticated RPC context and recorded in the
// event log instead — never trusted from disk.
type Manifest struct {
	Version        string    `json:"version"`
	AllowDowngrade bool      `json:"allowDowngrade"`
	StagedAt       time.Time `json:"stagedAt"`
}

// Stale reports whether the manifest is too old to act on.
func (m *Manifest) Stale(now time.Time) bool {
	return now.Sub(m.StagedAt) > manifestMaxAge
}

// Result is the applier's terminal record, written to Workdir/ResultName on
// every apply — success included — so runed can emit the matching event
// within seconds and the CLI can tell "rolled back" from "never fired".
type Result struct {
	// Outcome: "success" | "rolled-back" | "failed" | "noop".
	Outcome     string    `json:"outcome"`
	FromVersion string    `json:"fromVersion"`
	ToVersion   string    `json:"toVersion"`
	Reason      string    `json:"reason,omitempty"`
	FinishedAt  time.Time `json:"finishedAt"`
}

// WriteResult writes the result file (root-owned, world-readable).
func WriteResult(r Result) error {
	if err := os.MkdirAll(Workdir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(Workdir, "."+ResultName+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(Workdir, ResultName))
}

// ReadResult loads the applier's result file. Callers that trust the
// content for anything (event emission) must first check the file is
// root-owned — see ResultOwnedByRoot.
func ReadResult() (*Result, os.FileInfo, error) {
	p := filepath.Join(Workdir, ResultName)
	fi, err := os.Stat(p)
	if err != nil {
		return nil, nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, nil, err
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	return &r, fi, nil
}
