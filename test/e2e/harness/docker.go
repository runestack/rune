//go:build e2e
// +build e2e

package harness

import (
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
		cmd := exec.Command("docker", "info")
		done := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			return
		}
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			dockerOK = err == nil
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
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
