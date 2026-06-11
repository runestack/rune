//go:build e2e
// +build e2e

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	buildErr  error

	runedBin string
	runeBin  string
)

// binaries builds runed and rune once per test process and returns
// their paths. The Go build cache makes repeat builds cheap, and
// always building (rather than reusing a stale bin/) guarantees the
// tests exercise the code under review.
func binaries(t *testing.T) (runed, runeCLI string) {
	t.Helper()
	buildOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			buildErr = err
			return
		}
		binDir := filepath.Join(root, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			buildErr = fmt.Errorf("create bin dir: %w", err)
			return
		}
		targets := []struct{ out, pkg string }{
			{filepath.Join(binDir, "runed"), "./cmd/runed"},
			{filepath.Join(binDir, "rune"), "./cmd/rune"},
		}
		for _, tgt := range targets {
			cmd := exec.Command("go", "build", "-o", tgt.out, tgt.pkg)
			cmd.Dir = root
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("build %s: %w\n%s", tgt.pkg, err, out)
				return
			}
		}
		runedBin = targets[0].out
		runeBin = targets[1].out
	})
	if buildErr != nil {
		t.Fatalf("harness: %v", buildErr)
	}
	return runedBin, runeBin
}

// moduleRoot walks up from the working directory to the directory
// containing go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}
