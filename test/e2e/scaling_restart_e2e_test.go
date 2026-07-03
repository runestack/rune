//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
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
//
// UPDATED for issue #140: restart is now one server-side template restamp, so
// the assertions strengthened — the desired scale must never LEAVE 1 (no
// drain-through-zero at all), and the instance must be genuinely replaced
// every time (the old detach path could silently no-op the bounce).
func TestRestartDetach_ReturnsToScale(t *testing.T) {
	ctx := harness.New(t)
	svc := generated.NewServiceServiceClient(ctx.Conn())
	inst := generated.NewInstanceServiceClient(ctx.Conn())

	castScaledService(t, ctx, &svc, "web", 1)

	const iterations = 3
	for i := 0; i < iterations; i++ {
		// castScaledService only waits for the desired scale to land; the
		// instance record may still be seconds away. Wait for it before
		// snapshotting the pre-restart IDs.
		var before map[string]bool
		ctx.Eventually(harness.DefaultConvergeTimeout, "live instance before restart", func() bool {
			before = liveInstanceIDs(ctx, inst, "web")
			return len(before) == 1
		})

		ctx.CLI.MustRun(t, "restart", "web", "--detach")

		// The restart must genuinely replace the instance.
		ctx.Eventually(harness.DefaultConvergeTimeout, "instance to be replaced after detached restart", func() bool {
			now := liveInstanceIDs(ctx, inst, "web")
			if len(now) != 1 {
				return false
			}
			for id := range now {
				if before[id] {
					return false // old instance still live
				}
			}
			return true
		})

		// The desired scale must never have been part of the restart.
		stableUntil := time.Now().Add(2 * time.Second)
		for time.Now().Before(stableUntil) {
			s, err := getScale(ctx, &svc, "web")
			if err != nil {
				t.Fatalf("iteration %d: get scale: %v", i, err)
			}
			if s != 1 {
				t.Fatalf("iteration %d: scale is %d — restart must not touch the desired scale", i, s)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// liveInstanceIDs returns the IDs of non-Deleted instances of a service.
func liveInstanceIDs(ctx *harness.Context, inst generated.InstanceServiceClient, service string) map[string]bool {
	c, cancel := ctx.Ctx()
	defer cancel()
	resp, err := inst.ListInstances(c, &generated.ListInstancesRequest{Namespace: "default"})
	if err != nil {
		return nil
	}
	ids := map[string]bool{}
	for _, in := range resp.GetInstances() {
		if in.GetServiceName() != service {
			continue
		}
		if in.GetStatus() == generated.InstanceStatus_INSTANCE_STATUS_DELETED {
			continue
		}
		ids[in.GetId()] = true
	}
	return ids
}

// TestRestart_ReplacesInstancesInPlace covers the synchronous restart (issue
// #140): the CLI must block until the instance has been replaced at the new
// template generation and be Running, exit 0 — and the desired scale must
// never be observed at 0 during the whole operation (the old implementation
// drained through zero).
func TestRestart_ReplacesInstancesInPlace(t *testing.T) {
	ctx := harness.New(t)
	svc := generated.NewServiceServiceClient(ctx.Conn())
	inst := generated.NewInstanceServiceClient(ctx.Conn())

	castScaledService(t, ctx, &svc, "api", 1)
	var before map[string]bool
	ctx.Eventually(harness.DefaultConvergeTimeout, "live instance before restart", func() bool {
		before = liveInstanceIDs(ctx, inst, "api")
		return len(before) == 1
	})

	// Watch the scale in the background during the restart: it must never
	// dip to 0.
	stopWatch := make(chan struct{})
	sawZero := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-stopWatch:
				return
			default:
			}
			if s, err := getScale(ctx, &svc, "api"); err == nil && s == 0 {
				select {
				case sawZero <- struct{}{}:
				default:
				}
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	res := ctx.CLI.MustRun(t, "restart", "api")
	close(stopWatch)

	select {
	case <-sawZero:
		t.Fatal("desired scale was observed at 0 during restart — restart must replace in place, not drain through zero")
	default:
	}

	// The synchronous CLI already waited: the instance must be replaced now.
	after := liveInstanceIDs(ctx, inst, "api")
	if len(after) != 1 {
		t.Fatalf("expected 1 live instance after restart, got %d", len(after))
	}
	for id := range after {
		if before[id] {
			t.Fatalf("instance %s survived the restart — replacement did not happen (CLI output: %s)", id, res.Stdout)
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

// TestParallelScaleRestartStatus_Converges is the RFC #129 Phase 3 acceptance
// test at the e2e level: 4 goroutines hammer ONE service for 15s with
// immediate scale ops (targets 1–3) and 0-then-N bounces, then a final
// ScaleService(2) is the authoritative desired state. The service must
// converge to scale 2 and HOLD it. (Since issue #140 `rune restart` no longer
// bounces the scale at all — the 0→N pattern here remains as a raw
// scaling-pipeline stress shape, not a model of restart.)
// All hammering is pure gRPC: t.Fatal is illegal off the test goroutine, and
// transient op-conflict errors during the storm are expected and ignored.
func TestParallelScaleRestartStatus_Converges(t *testing.T) {
	ctx := harness.New(t)
	svc := generated.NewServiceServiceClient(ctx.Conn())

	castScaledService(t, ctx, &svc, "storm", 2)

	scaleTo := func(target int) {
		c, cancel := ctx.Ctx()
		defer cancel()
		// Best-effort: rejections/conflicts under the storm are part of the test.
		_, _ = svc.ScaleService(c, &generated.ScaleServiceRequest{
			Name:      "storm",
			Namespace: "default",
			Scale:     int32(target),
			Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
		})
	}

	const hammerFor = 15 * time.Second
	deadline := time.Now().Add(hammerFor)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				if g%2 == 0 {
					// Scale cycler: targets 1..3.
					scaleTo(1 + (i % 3))
				} else {
					// Restart bounce: 0 then back (the `restart --detach` shape).
					scaleTo(0)
					scaleTo(1 + (i % 3))
				}
				time.Sleep(time.Duration(75+g*40) * time.Millisecond)
			}
		}(g)
	}
	wg.Wait()

	// Authoritative final desired state.
	const finalTarget = 2
	c, cancel := ctx.Ctx()
	if _, err := svc.ScaleService(c, &generated.ScaleServiceRequest{
		Name:      "storm",
		Namespace: "default",
		Scale:     int32(finalTarget),
		Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
	}); err != nil {
		cancel()
		t.Fatalf("final scale to %d: %v", finalTarget, err)
	}
	cancel()

	// Converge...
	ctx.Eventually(harness.DefaultConvergeTimeout, "storm to settle at the final target", func() bool {
		s, err := getScale(ctx, &svc, "storm")
		return err == nil && s == finalTarget
	})

	// ...and HOLD (no late scale write may knock it off the target).
	stableUntil := time.Now().Add(3 * time.Second)
	for time.Now().Before(stableUntil) {
		s, err := getScale(ctx, &svc, "storm")
		if err != nil {
			t.Fatalf("get scale after storm: %v", err)
		}
		if s != finalTarget {
			t.Fatalf("scale fell to %d after settling at %d (lost update under load)", s, finalTarget)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
