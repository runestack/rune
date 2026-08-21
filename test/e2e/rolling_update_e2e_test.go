//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/test/e2e/harness"
)

// RUNE-042 acceptance: a deploy must not drop traffic.
//
// What this measures, precisely. The dataplane publishes an endpoint for every
// instance whose status is Running and for no others (republishService filters
// on exactly that), so the count of Running instances IS the size of the
// service's endpoint set — the set of containers the load balancer will hand
// the next connection to. Sampling it at high frequency across a real deploy,
// with real containers and the real drain, measures the thing the milestone
// claims: capacity to serve never disappears.
//
// What it does not measure: individual request outcomes. That needs traffic
// through the VIP, which means the nftables dataplane, which means Linux — a
// request-level test would skip on most developer machines and give a false
// sense of coverage. The endpoint-set invariant is platform-independent, is
// the same property a request-level test would be asserting underneath, and
// fails loudly on the regression that matters: before RUNE-042 a template
// change took every instance down at once, which this samples as a floor
// violation within milliseconds.

// endpointSetSampler polls the size of a service's endpoint set in the
// background, recording the minimum it ever observes.
type endpointSetSampler struct {
	mu       sync.Mutex
	min      int
	samples  int
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func sampleEndpointSet(t *testing.T, ctx *harness.Context, inst generated.InstanceServiceClient, service string, every time.Duration) *endpointSetSampler {
	t.Helper()
	s := &endpointSetSampler{min: 1 << 30, stopCh: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-tick.C:
				n := runningInstanceCount(t, ctx, inst, service)
				if n < 0 {
					continue // transient API error; not an availability signal
				}
				s.mu.Lock()
				if n < s.min {
					s.min = n
				}
				s.samples++
				s.mu.Unlock()
			}
		}
	}()
	return s
}

func (s *endpointSetSampler) stop() (minSeen, samples int) {
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.min, s.samples
}

// maxRunningGeneration is the newest template generation any Running replica
// of the service currently carries.
func maxRunningGeneration(t *testing.T, ctx *harness.Context, inst generated.InstanceServiceClient, service string) int64 {
	t.Helper()
	c, cancel := ctx.Ctx()
	defer cancel()
	resp, err := inst.ListInstances(c, &generated.ListInstancesRequest{Namespace: "default"})
	if err != nil {
		return 0
	}
	var max int64
	for _, in := range resp.GetInstances() {
		if in.GetServiceName() != service || in.GetStatus() != generated.InstanceStatus_INSTANCE_STATUS_RUNNING {
			continue
		}
		if g := int64(in.GetMetadata().GetGeneration()); g > max {
			max = g
		}
	}
	return max
}

func writeService(t *testing.T, dir, name, image string, scale int) string {
	t.Helper()
	file := filepath.Join(dir, name+".yaml")
	body := fmt.Sprintf(`
service:
  name: %s
  image: %s
  scale: %d
  drainSeconds: 1
  ports:
    - name: http
      port: 80
  health:
    readiness:
      type: tcp
      port: 80
      intervalSeconds: 1
      timeoutSeconds: 1
      failureThreshold: 1
      successThreshold: 1
`, name, image, scale)
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatalf("write service file: %v", err)
	}
	return file
}

// TestRollingUpdate_CastKeepsCapacity is the milestone's acceptance test: a
// template change on a multi-replica service must never reduce the endpoint
// set below the desired scale.
//
// Pre-RUNE-042 this fails immediately — a template change deleted every
// instance in one reconcile pass, so the sampler sees 0.
func TestRollingUpdate_CastKeepsCapacity(t *testing.T) {
	harness.RequireDocker(t)
	ctx := harness.New(t)

	const (
		name  = "rolling-cast"
		scale = 3
	)
	dir := t.TempDir()
	inst := generated.NewInstanceServiceClient(ctx.Conn())

	// Land the service first. Image pull dominates a cold runner.
	ctx.CLI.MustRun(t, "cast", writeService(t, dir, name, "nginx:1.27-alpine", scale), "--detach", "--release", name)
	ctx.Eventually(5*time.Minute, "initial replicas to be Running", func() bool {
		return runningInstanceCount(t, ctx, inst, name) == scale
	})

	// Record the template every replica is on now, so convergence can be
	// asserted against "strictly newer" rather than against an image string.
	preGen := maxRunningGeneration(t, ctx, inst, name)

	// Now change the template and watch capacity throughout.
	sampler := sampleEndpointSet(t, ctx, inst, name, 50*time.Millisecond)
	ctx.CLI.MustRun(t, "cast", writeService(t, dir, name, "nginx:1.28-alpine", scale), "--detach", "--release", name)

	ctx.Eventually(5*time.Minute, "every replica to reach the new template", func() bool {
		c, cancel := ctx.Ctx()
		defer cancel()
		resp, err := inst.ListInstances(c, &generated.ListInstancesRequest{Namespace: "default"})
		if err != nil {
			return false
		}
		running, onNew := 0, 0
		for _, in := range resp.GetInstances() {
			if in.GetServiceName() != name || in.GetStatus() != generated.InstanceStatus_INSTANCE_STATUS_RUNNING {
				continue
			}
			running++
			if int64(in.GetMetadata().GetGeneration()) > preGen {
				onNew++
			}
		}
		return running == scale && onNew == scale
	})

	minSeen, samples := sampler.stop()
	if samples < 10 {
		t.Fatalf("sampler collected only %d samples — too few to trust this result", samples)
	}
	if minSeen < scale {
		t.Fatalf("endpoint set fell to %d during the update (want >= %d across %d samples): "+
			"the deploy dropped traffic-carrying capacity", minSeen, scale, samples)
	}
	t.Logf("endpoint set never fell below %d across %d samples", minSeen, samples)

	ctx.CLI.MustRun(t, "delete", "service", name, "--force")
	ctx.Eventually(harness.DefaultConvergeTimeout, "instances to be gone", func() bool {
		return runningInstanceCount(t, ctx, inst, name) == 0
	})
}

// TestRollingUpdate_RestartKeepsCapacity: `rune restart` is a template
// restamp, so it must roll exactly like a cast. This is the case the docs
// promised for two releases and the product did not have.
func TestRollingUpdate_RestartKeepsCapacity(t *testing.T) {
	harness.RequireDocker(t)
	ctx := harness.New(t)

	const (
		name  = "rolling-restart"
		scale = 3
	)
	inst := generated.NewInstanceServiceClient(ctx.Conn())

	ctx.CLI.MustRun(t, "cast", writeService(t, t.TempDir(), name, "nginx:1.27-alpine", scale), "--detach", "--release", name)
	ctx.Eventually(5*time.Minute, "initial replicas to be Running", func() bool {
		return runningInstanceCount(t, ctx, inst, name) == scale
	})

	sampler := sampleEndpointSet(t, ctx, inst, name, 50*time.Millisecond)
	ctx.CLI.MustRun(t, "restart", name, "--timeout", "5m")
	minSeen, samples := sampler.stop()

	if samples < 10 {
		t.Fatalf("sampler collected only %d samples — too few to trust this result", samples)
	}
	if minSeen < scale {
		t.Fatalf("endpoint set fell to %d during restart (want >= %d across %d samples)", minSeen, scale, samples)
	}
	t.Logf("endpoint set never fell below %d across %d samples", minSeen, samples)

	ctx.CLI.MustRun(t, "delete", "service", name, "--force")
	ctx.Eventually(harness.DefaultConvergeTimeout, "instances to be gone", func() bool {
		return runningInstanceCount(t, ctx, inst, name) == 0
	})
}

// TestRollingUpdate_RecreateStillTakesEveryoneDown pins the opt-out. A service
// that declares `updateStrategy: recreate` must keep the pre-RUNE-042
// behaviour exactly — this is what a single-writer workload relies on, so it
// needs a test proving the escape hatch is real and not just accepted syntax.
func TestRollingUpdate_RecreateStillTakesEveryoneDown(t *testing.T) {
	harness.RequireDocker(t)
	ctx := harness.New(t)

	const (
		name  = "recreate-svc"
		scale = 2
	)
	dir := t.TempDir()
	inst := generated.NewInstanceServiceClient(ctx.Conn())

	spec := func(image string) string {
		file := filepath.Join(dir, name+".yaml")
		body := fmt.Sprintf(`
service:
  name: %s
  image: %s
  scale: %d
  drainSeconds: 1
  updateStrategy: recreate
`, name, image, scale)
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatalf("write service file: %v", err)
		}
		return file
	}

	ctx.CLI.MustRun(t, "cast", spec("nginx:1.27-alpine"), "--detach", "--release", name)
	ctx.Eventually(5*time.Minute, "initial replicas to be Running", func() bool {
		return runningInstanceCount(t, ctx, inst, name) == scale
	})

	sampler := sampleEndpointSet(t, ctx, inst, name, 50*time.Millisecond)
	ctx.CLI.MustRun(t, "cast", spec("nginx:1.28-alpine"), "--detach", "--release", name)
	ctx.Eventually(5*time.Minute, "replicas to return on the new template", func() bool {
		return runningInstanceCount(t, ctx, inst, name) == scale
	})
	minSeen, _ := sampler.stop()

	// The point of recreate is that it DOES take everything down. If capacity
	// never dipped, the strategy was ignored and a single-writer workload
	// would be running two copies without knowing it.
	if minSeen >= scale {
		t.Fatalf("capacity never dipped below %d: `updateStrategy: recreate` did not take instances "+
			"down before replacing them, so the exclusivity it promises is not real", scale)
	}
	t.Logf("recreate dipped to %d as expected", minSeen)

	ctx.CLI.MustRun(t, "delete", "service", name, "--force")
	ctx.Eventually(harness.DefaultConvergeTimeout, "instances to be gone", func() bool {
		return runningInstanceCount(t, ctx, inst, name) == 0
	})
}
