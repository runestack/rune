//go:build e2e
// +build e2e

package harness

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"
)

var (
	dockerOnce sync.Once
	dockerOK   bool
)

// DockerAvailable reports whether a Docker daemon answers, probing at
// most once per test process.
func DockerAvailable() bool {
	dockerOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		dockerOK = exec.CommandContext(ctx, "docker", "info").Run() == nil
	})
	return dockerOK
}

// RequireDocker skips the test when no Docker daemon is reachable.
// Dataplane tests (instances actually running) call this first;
// control-plane tests should not, so they keep running on macOS
// laptops and CI runners without Docker.
func RequireDocker(t *testing.T) {
	t.Helper()
	if !DockerAvailable() {
		t.Skip("skipping: Docker daemon not available (dataplane E2E needs it)")
	}
}
