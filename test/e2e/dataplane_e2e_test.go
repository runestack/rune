//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/test/e2e/harness"
)

// runningInstanceCount returns how many instances of a service are Running.
func runningInstanceCount(t *testing.T, ctx *harness.Context, inst generated.InstanceServiceClient, service string) int {
	t.Helper()
	c, cancel := ctx.Ctx()
	defer cancel()
	resp, err := inst.ListInstances(c, &generated.ListInstancesRequest{Namespace: "default"})
	if err != nil {
		return -1
	}
	n := 0
	for _, in := range resp.GetInstances() {
		if in.GetServiceName() == service && in.GetStatus() == generated.InstanceStatus_INSTANCE_STATUS_RUNNING {
			n++
		}
	}
	return n
}

// TestDataplane_RestartReturnsToRunning is the dataplane regression for the
// reported restart flake: a synchronous `rune restart` with a real running
// container drives the service 1 → 0 → 1, exercising the actual drain timing
// where the scaling-operation race window is widest. The service must come
// back to exactly one Running instance every time — not strand at 0. Looped,
// because the failure was nondeterministic. Skipped without Docker.
func TestDataplane_RestartReturnsToRunning(t *testing.T) {
	harness.RequireDocker(t)
	ctx := harness.New(t)

	svcFile := filepath.Join(t.TempDir(), "web.yaml")
	if err := os.WriteFile(svcFile, []byte(`
service:
  name: web-rs
  image: nginx:alpine
  scale: 1
`), 0o644); err != nil {
		t.Fatalf("write service file: %v", err)
	}
	ctx.CLI.MustRun(t, "cast", svcFile, "--detach", "--release", "web-rs")

	inst := generated.NewInstanceServiceClient(ctx.Conn())
	// Image pull dominates the first convergence on a cold runner.
	ctx.Eventually(3*time.Minute, "web-rs to be Running before restart", func() bool {
		return runningInstanceCount(t, ctx, inst, "web-rs") == 1
	})

	const restarts = 3
	for i := 0; i < restarts; i++ {
		// Synchronous restart with a bounded budget: drain to 0 then back to 1.
		// On the pre-fix race this strands at 0, so the start phase never
		// reaches 1 Running and restart exits non-zero (MustRun fails the test)
		// rather than hanging the suite.
		ctx.CLI.MustRun(t, "restart", "web-rs", "--timeout", "120s")

		// After restart returns, the service must be back to exactly one
		// Running instance — and stay there (guard a late down-write).
		ctx.Eventually(harness.DefaultConvergeTimeout, "web-rs to be Running after restart", func() bool {
			return runningInstanceCount(t, ctx, inst, "web-rs") == 1
		})
		stableUntil := time.Now().Add(3 * time.Second)
		for time.Now().Before(stableUntil) {
			if n := runningInstanceCount(t, ctx, inst, "web-rs"); n != 1 {
				t.Fatalf("restart %d: expected 1 Running instance, got %d (restart race stranded the service)", i, n)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// Tear down so the container does not outlive the test.
	ctx.CLI.MustRun(t, "delete", "service", "web-rs", "--force")
	ctx.Eventually(harness.DefaultConvergeTimeout, "web-rs instances to be gone", func() bool {
		return runningInstanceCount(t, ctx, inst, "web-rs") == 0
	})
}

// TestDataplane_RestartEarlyDrainTimeoutRecovers exercises the realistic
// trigger for "restart stranded my service at 0": the drain phase gives up
// early (slow-stopping container, or a short --drain-timeout), so the CLI's
// scale-back-up fires while the scale-DOWN is still in flight — exactly the
// back-to-back down→up that the scaling pipeline must handle. The restart
// command itself may exit non-zero (the drain timed out), but the service must
// NOT be left stranded at 0: it must self-heal back to one Running instance.
func TestDataplane_RestartEarlyDrainTimeoutRecovers(t *testing.T) {
	harness.RequireDocker(t)
	ctx := harness.New(t)

	svcFile := filepath.Join(t.TempDir(), "web.yaml")
	if err := os.WriteFile(svcFile, []byte(`
service:
  name: web-rt
  image: nginx:alpine
  scale: 1
`), 0o644); err != nil {
		t.Fatalf("write service file: %v", err)
	}
	ctx.CLI.MustRun(t, "cast", svcFile, "--detach", "--release", "web-rt")

	inst := generated.NewInstanceServiceClient(ctx.Conn())
	ctx.Eventually(3*time.Minute, "web-rt to be Running before restart", func() bool {
		return runningInstanceCount(t, ctx, inst, "web-rt") == 1
	})

	// Force the drain wait to give up almost immediately, so the scale-back-up
	// races the in-flight scale-down. The command may exit non-zero — that's
	// fine; we only require that the service recovers rather than stranding.
	_ = ctx.CLI.Run(t, "restart", "web-rt", "--drain-timeout", "1ms", "--timeout", "40s")

	ctx.Eventually(2*time.Minute, "web-rt to self-heal back to 1 Running after an early-drain restart", func() bool {
		return runningInstanceCount(t, ctx, inst, "web-rt") == 1
	})
	// And hold, to catch a late scale-down write winning the race.
	stableUntil := time.Now().Add(3 * time.Second)
	for time.Now().Before(stableUntil) {
		if n := runningInstanceCount(t, ctx, inst, "web-rt"); n != 1 {
			t.Fatalf("expected 1 Running after recovery, got %d (restart stranded the service)", n)
		}
		time.Sleep(200 * time.Millisecond)
	}

	ctx.CLI.MustRun(t, "delete", "service", "web-rt", "--force")
	ctx.Eventually(harness.DefaultConvergeTimeout, "web-rt instances to be gone", func() bool {
		return runningInstanceCount(t, ctx, inst, "web-rt") == 0
	})
}

// TestDataplane_InstanceRuns is the Tier-2 test: with a Docker daemon
// present, a cast service must converge to an actually-running
// container. Skipped automatically when Docker is unavailable.
func TestDataplane_InstanceRuns(t *testing.T) {
	harness.RequireDocker(t)
	ctx := harness.New(t)

	svcFile := filepath.Join(t.TempDir(), "web.yaml")
	if err := os.WriteFile(svcFile, []byte(`
service:
  name: web-dp
  image: nginx:alpine
  scale: 1
`), 0o644); err != nil {
		t.Fatalf("write service file: %v", err)
	}
	ctx.CLI.MustRun(t, "cast", svcFile, "--detach", "--release", "web-dp")

	instClient := generated.NewInstanceServiceClient(ctx.Conn())
	// Image pull dominates this window on cold runners.
	ctx.Eventually(3*time.Minute, "an instance of web-dp to be Running", func() bool {
		c, cancel := ctx.Ctx()
		defer cancel()
		resp, err := instClient.ListInstances(c, &generated.ListInstancesRequest{Namespace: "default"})
		if err != nil {
			return false
		}
		for _, inst := range resp.GetInstances() {
			if inst.GetServiceName() == "web-dp" && inst.GetStatus() == generated.InstanceStatus_INSTANCE_STATUS_RUNNING {
				return true
			}
		}
		return false
	})

	// Tear down so the container does not outlive the test.
	ctx.CLI.MustRun(t, "delete", "service", "web-dp", "--force")
	ctx.Eventually(harness.DefaultConvergeTimeout, "instances to be gone", func() bool {
		c, cancel := ctx.Ctx()
		defer cancel()
		resp, err := instClient.ListInstances(c, &generated.ListInstancesRequest{Namespace: "default"})
		if err != nil {
			return false
		}
		for _, inst := range resp.GetInstances() {
			if inst.GetServiceName() == "web-dp" && inst.GetStatus() == generated.InstanceStatus_INSTANCE_STATUS_RUNNING {
				return false
			}
		}
		return true
	})
}
