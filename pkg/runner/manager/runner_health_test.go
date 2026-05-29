package manager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
)

// fakeHealthRunner embeds the Runner interface (nil) so it satisfies the
// type without implementing every method, and adds HealthCheck so it also
// satisfies runner.HealthChecker — which is all RunnerHealth exercises.
type fakeHealthRunner struct {
	runner.Runner
	err error
}

func (f fakeHealthRunner) HealthCheck(context.Context) error { return f.err }

// A runner that does NOT implement HealthChecker (like the process runner)
// must report "ready" — it has no daemon to probe.
type bareRunner struct{ runner.Runner }

func TestRunnerHealth_DockerReachableAndDown(t *testing.T) {
	m := NewRunnerManager(log.NewLogger())
	m.initialized = true

	m.dockerRunner = fakeHealthRunner{}
	if got := m.RunnerHealth(context.Background())["docker"]; got != "ready" {
		t.Fatalf("reachable docker = %q, want ready", got)
	}

	m.dockerRunner = fakeHealthRunner{err: errors.New("dial tcp /var/run/docker.sock: connection refused")}
	got := m.RunnerHealth(context.Background())["docker"]
	if !strings.HasPrefix(got, "unreachable:") || !strings.Contains(got, "connection refused") {
		t.Fatalf("down docker = %q, want unreachable: …connection refused", got)
	}
}

func TestRunnerHealth_NonHealthCheckerIsReady(t *testing.T) {
	m := NewRunnerManager(log.NewLogger())
	m.initialized = true
	m.processRunner = bareRunner{}
	if got := m.RunnerHealth(context.Background())["process"]; got != "ready" {
		t.Fatalf("process = %q, want ready (no HealthChecker => assumed ready)", got)
	}
}

func TestRunnerHealth_OnlyConfiguredRunners(t *testing.T) {
	m := NewRunnerManager(log.NewLogger())
	m.initialized = true
	m.dockerRunner = fakeHealthRunner{}
	h := m.RunnerHealth(context.Background())
	if _, ok := h["process"]; ok {
		t.Fatalf("unconfigured process runner should not appear: %v", h)
	}
	if _, ok := h["docker"]; !ok {
		t.Fatalf("configured docker runner should appear: %v", h)
	}
}
