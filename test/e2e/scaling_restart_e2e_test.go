//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/test/e2e/harness"
)

// castScaledService casts a single nginx service at the given scale in detached
// mode (no container runtime needed — assertions stay on control-plane state)
// and waits for the desired scale to land.
func castScaledService(t *testing.T, ctx *harness.Context, svc *generated.ServiceServiceClient, name string, scale int) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, name+".yaml")
	body := "service:\n  name: " + name + "\n  image: nginx:alpine\n  scale: " + strconv.Itoa(scale) + "\n"
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatalf("write service file: %v", err)
	}
	ctx.CLI.MustRun(t, "cast", file, "--detach", "--release", name)
	ctx.Eventually(harness.DefaultConvergeTimeout, "initial scale to land", func() bool {
		s, err := getScale(ctx, svc, name)
		return err == nil && s == scale
	})
}

func getScale(ctx *harness.Context, svc *generated.ServiceServiceClient, name string) (int, error) {
	c, cancel := ctx.Ctx()
	defer cancel()
	resp, err := (*svc).GetService(c, &generated.GetServiceRequest{Name: name, Namespace: "default"})
	if err != nil {
		return 0, err
	}
	return int(resp.GetService().GetScale()), nil
}

// TestRestartDetach_ReturnsToScale is the regression test for the reported
// flake: `rune restart` drives the service 1 → 0 and then hangs at 0, so the
// operator has to restart a second time to bring it back. The detached restart
// path fires ScaleService(0) immediately followed by ScaleService(current); if
// the desired scale ever settles at 0 instead of returning to the original
// value, the service is stranded "stopped" with no signal.
//
// Looped, because the failure is a goroutine race in how scaling operations are
// applied — it does not reproduce every single time.
func TestRestartDetach_ReturnsToScale(t *testing.T) {
	ctx := harness.New(t)
	svc := generated.NewServiceServiceClient(ctx.Conn())

	castScaledService(t, ctx, &svc, "web", 1)

	const iterations = 5
	for i := 0; i < iterations; i++ {
		ctx.CLI.MustRun(t, "restart", "web", "--detach")

		// The desired scale must return to 1. If the restart race strands the
		// service at 0, this never becomes true and the test fails (rather than
		// hanging) at the timeout.
		ctx.Eventually(harness.DefaultConvergeTimeout, "scale to return to 1 after restart", func() bool {
			s, err := getScale(ctx, &svc, "web")
			return err == nil && s == 1
		})

		// And it must STAY at 1 — guard against a late scale-down write winning
		// the race and knocking it back to 0 after we observed 1.
		stableUntil := time.Now().Add(2 * time.Second)
		for time.Now().Before(stableUntil) {
			s, err := getScale(ctx, &svc, "web")
			if err != nil {
				t.Fatalf("iteration %d: get scale: %v", i, err)
			}
			if s != 1 {
				t.Fatalf("iteration %d: scale fell back to %d after returning to 1 (restart race stranded the service)", i, s)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// TestScaleDownUp_RapidReturnsToTarget isolates the orchestrator from the CLI:
// it drives ScaleService(0) immediately followed by ScaleService(1) over gRPC,
// the same back-to-back sequence `rune restart` issues, and asserts the desired
// scale converges to (and stays at) the up target. This pins the race to the
// scaling pipeline rather than CLI orchestration.
func TestScaleDownUp_RapidReturnsToTarget(t *testing.T) {
	ctx := harness.New(t)
	svc := generated.NewServiceServiceClient(ctx.Conn())

	castScaledService(t, ctx, &svc, "api", 1)

	scaleTo := func(target int) {
		c, cancel := ctx.Ctx()
		defer cancel()
		if _, err := svc.ScaleService(c, &generated.ScaleServiceRequest{
			Name:      "api",
			Namespace: "default",
			Scale:     int32(target),
			Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
		}); err != nil {
			t.Fatalf("scale to %d: %v", target, err)
		}
	}

	const iterations = 5
	for i := 0; i < iterations; i++ {
		scaleTo(0)
		scaleTo(1)
		ctx.Eventually(harness.DefaultConvergeTimeout, "rapid down→up to settle at 1", func() bool {
			s, err := getScale(ctx, &svc, "api")
			return err == nil && s == 1
		})
		// Hold to catch a late down-write winning the race.
		stableUntil := time.Now().Add(2 * time.Second)
		for time.Now().Before(stableUntil) {
			s, err := getScale(ctx, &svc, "api")
			if err != nil {
				t.Fatalf("iteration %d: get scale: %v", i, err)
			}
			if s != 1 {
				t.Fatalf("iteration %d: scale settled at %d, not 1 (down→up race)", i, s)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}
