package health

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	instancectl "github.com/runestack/rune/pkg/orchestrator/instance"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readinessResult(ok bool) types.HealthCheckResult {
	return types.HealthCheckResult{Success: ok, CheckTime: time.Now(), CheckType: "readiness"}
}

func livenessResult(ok bool) types.HealthCheckResult {
	return types.HealthCheckResult{Success: ok, CheckTime: time.Now(), CheckType: "liveness"}
}

func storedReady(ctx context.Context, t *testing.T, c *healthController, inst *types.Instance) (ready bool, recorded bool) {
	t.Helper()
	var got types.Instance
	require.NoError(t, c.store.Get(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &got))
	return got.Ready, got.ReadyChangedAt != nil
}

// A Running instance whose readiness probe keeps failing is marked not ready
// after failureThreshold failures, and ready again only after
// successThreshold consecutive passes. Status stays Running throughout.
func TestReadinessThresholdsFlipPersistedReady(t *testing.T) {
	baseCtx, testStore, _, hc := setupHealthController(t)
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	require.NoError(t, hc.Start(ctx))
	defer hc.Stop()
	c := hc.(*healthController)

	service := &types.Service{
		ID: "api", Name: "api", Namespace: "default", Runtime: "container",
		Health: &types.HealthCheck{Readiness: &types.Probe{
			Type: "http", Port: 80, FailureThreshold: 2, SuccessThreshold: 2,
		}},
	}
	require.NoError(t, testStore.CreateService(ctx, service))
	inst := &types.Instance{
		ID: "api-0", Name: "api-0", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status: types.InstanceStatusRunning, CreatedAt: time.Now(),
	}
	require.NoError(t, testStore.CreateInstance(ctx, inst))
	ih := seedHealthEntry(c, inst, service)
	ih.ready = inst.IsReady()
	ih.promoted = true

	c.updateHealthStatus(inst.ID, readinessResult(false), "readiness")
	_, recorded := storedReady(ctx, t, c, inst)
	assert.False(t, recorded, "one failure is below failureThreshold")

	c.updateHealthStatus(inst.ID, readinessResult(false), "readiness")
	require.Eventually(t, func() bool {
		ready, recorded := storedReady(ctx, t, c, inst)
		return recorded && !ready
	}, 3*time.Second, 10*time.Millisecond, "failureThreshold failures must record not-ready")

	c.updateHealthStatus(inst.ID, readinessResult(true), "readiness")
	time.Sleep(50 * time.Millisecond)
	ready, _ := storedReady(ctx, t, c, inst)
	assert.False(t, ready, "one pass is below successThreshold")

	c.updateHealthStatus(inst.ID, readinessResult(true), "readiness")
	require.Eventually(t, func() bool {
		ready, _ := storedReady(ctx, t, c, inst)
		return ready
	}, 3*time.Second, 10*time.Millisecond, "successThreshold passes must record ready")

	var got types.Instance
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", inst.ID, &got))
	assert.Equal(t, types.InstanceStatusRunning, got.Status, "readiness never changes Status")
}

// Readiness verdicts are not recorded before promotion: Starting→Running is
// the readiness gate there.
func TestReadinessThresholdsIgnoreStartingInstances(t *testing.T) {
	baseCtx, testStore, _, hc := setupHealthController(t)
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	require.NoError(t, hc.Start(ctx))
	defer hc.Stop()
	c := hc.(*healthController)

	service := &types.Service{
		ID: "api", Name: "api", Namespace: "default", Runtime: "container",
		Health: &types.HealthCheck{Readiness: &types.Probe{Type: "http", Port: 80, FailureThreshold: 1}},
	}
	require.NoError(t, testStore.CreateService(ctx, service))
	inst := &types.Instance{
		ID: "api-0", Name: "api-0", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status: types.InstanceStatusStarting, CreatedAt: time.Now(),
	}
	require.NoError(t, testStore.CreateInstance(ctx, inst))
	ih := seedHealthEntry(c, inst, service)
	ih.ready = inst.IsReady()

	c.updateHealthStatus(inst.ID, readinessResult(false), "readiness")
	c.updateHealthStatus(inst.ID, readinessResult(false), "readiness")
	time.Sleep(50 * time.Millisecond)
	_, recorded := storedReady(ctx, t, c, inst)
	assert.False(t, recorded)
}

// After a runed restart the monitor entry is rebuilt with promoted=false. A
// Running instance that was withdrawn must still come back after
// successThreshold passes: the promotion path must not mark it ready in
// memory without writing anything.
func TestReadiness_WithdrawnInstanceRecoversAfterRestart(t *testing.T) {
	baseCtx, testStore, _, hc := setupHealthController(t)
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	require.NoError(t, hc.Start(ctx))
	defer hc.Stop()
	c := hc.(*healthController)

	service := &types.Service{
		ID: "api", Name: "api", Namespace: "default", Runtime: "container",
		Health: &types.HealthCheck{Readiness: &types.Probe{Type: "http", Port: 80, SuccessThreshold: 2}},
	}
	require.NoError(t, testStore.CreateService(ctx, service))
	changed := time.Now()
	inst := &types.Instance{
		ID: "api-0", Name: "api-0", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status: types.InstanceStatusRunning, CreatedAt: time.Now(),
		Ready: false, ReadyChangedAt: &changed,
	}
	require.NoError(t, testStore.CreateInstance(ctx, inst))
	ih := seedHealthEntry(c, inst, service)
	ih.ready = inst.IsReady() // as AddInstance seeds it; promoted stays false

	c.updateHealthStatus(inst.ID, readinessResult(true), "readiness")
	c.updateHealthStatus(inst.ID, readinessResult(true), "readiness")
	require.Eventually(t, func() bool {
		ready, _ := storedReady(ctx, t, c, inst)
		return ready
	}, 3*time.Second, 10*time.Millisecond, "successThreshold passes must restore a withdrawn instance")
}

// The stored verdict wins over the in-memory one: a write that failed, landed
// out of order or was overwritten is healed on the next probe.
func TestReadiness_ResyncsFromStoreBeforeEachProbe(t *testing.T) {
	baseCtx, testStore, _, hc := setupHealthController(t)
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	require.NoError(t, hc.Start(ctx))
	defer hc.Stop()
	c := hc.(*healthController)

	probe := &types.Probe{Type: "no-such-probe-type", Port: 80}
	service := &types.Service{
		ID: "api", Name: "api", Namespace: "default", Runtime: "container",
		Health: &types.HealthCheck{Readiness: probe},
	}
	require.NoError(t, testStore.CreateService(ctx, service))
	inst := &types.Instance{
		ID: "api-0", Name: "api-0", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status: types.InstanceStatusRunning, CreatedAt: time.Now(),
	}
	require.NoError(t, testStore.CreateInstance(ctx, inst))
	ih := seedHealthEntry(c, inst, service)
	ih.ready, ih.promoted = true, true

	// Someone else's write says not ready.
	changed := time.Now()
	inst.Ready, inst.ReadyChangedAt = false, &changed
	require.NoError(t, testStore.Update(ctx, types.ResourceTypeInstance, "default", inst.ID, inst))

	c.performHealthCheck(inst.ID, probe, "readiness")
	c.mu.RLock()
	defer c.mu.RUnlock()
	assert.False(t, ih.ready, "one failure is below the threshold, so only the resync can explain this")
}

type readinessEvents struct {
	mu     sync.Mutex
	events []types.Event
}

func (l *readinessEvents) Emit(_ context.Context, e types.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return nil
}
func (l *readinessEvents) ListByResource(context.Context, string, string, string, int) ([]types.Event, error) {
	return nil, nil
}
func (l *readinessEvents) ListSince(context.Context, int64, int) ([]types.Event, error) {
	return nil, nil
}
func (l *readinessEvents) LoadCursor(context.Context, string) (int64, error) { return 0, nil }
func (l *readinessEvents) SaveCursor(context.Context, string, int64) error   { return nil }

func (l *readinessEvents) reasons() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.events {
		out = append(out, e.Reason)
	}
	return out
}

// A withdrawal and a return are recorded as events, so operators can see
// them without reading logs.
func TestReadiness_FlipsEmitEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := store.NewTestStore()
	rm := manager.NewTestRunnerManager(nil)
	tr := runner.NewTestRunner()
	rm.SetDockerRunner(tr)
	rm.SetProcessRunner(tr)
	events := &readinessEvents{}
	ic := instancectl.NewController(st, rm, log.NewLogger(), instancectl.WithEventLog(events))
	hc := NewController(log.NewLogger(), st, rm, ic)
	require.NoError(t, hc.Start(ctx))
	defer hc.Stop()
	c := hc.(*healthController)

	service := &types.Service{
		ID: "api", Name: "api", Namespace: "default", Runtime: "container",
		Health: &types.HealthCheck{Readiness: &types.Probe{Type: "http", Port: 80, FailureThreshold: 1}},
	}
	require.NoError(t, st.CreateService(ctx, service))
	inst := &types.Instance{
		ID: "api-0", Name: "api-0", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status: types.InstanceStatusRunning, CreatedAt: time.Now(),
	}
	require.NoError(t, st.CreateInstance(ctx, inst))
	ih := seedHealthEntry(c, inst, service)
	ih.ready, ih.promoted = true, true

	c.updateHealthStatus(inst.ID, readinessResult(false), "readiness")
	require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]string{"NotReady"}, events.reasons()) },
		3*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		c.mu.RLock()
		defer c.mu.RUnlock()
		return !ih.readyWriting
	}, 3*time.Second, 10*time.Millisecond)

	c.updateHealthStatus(inst.ID, readinessResult(true), "readiness")
	require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]string{"NotReady", "Ready"}, events.reasons()) },
		3*time.Second, 10*time.Millisecond)
}

// Probe results that don't cross a threshold write nothing: rewriting
// ReadyChangedAt on every probe would restart the planner's minimum-ready
// window forever.
func TestReadiness_NonFlipWritesNothing(t *testing.T) {
	baseCtx, testStore, _, hc := setupHealthController(t)
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	require.NoError(t, hc.Start(ctx))
	defer hc.Stop()
	c := hc.(*healthController)

	service := &types.Service{
		ID: "api", Name: "api", Namespace: "default", Runtime: "container",
		Health: &types.HealthCheck{Readiness: &types.Probe{Type: "http", Port: 80}},
	}
	require.NoError(t, testStore.CreateService(ctx, service))
	changed := time.Now().Add(-time.Hour).Truncate(time.Second)
	inst := &types.Instance{
		ID: "api-0", Name: "api-0", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status: types.InstanceStatusRunning, CreatedAt: time.Now(),
		Ready: true, ReadyChangedAt: &changed,
	}
	require.NoError(t, testStore.CreateInstance(ctx, inst))

	t.Run("in memory: already ready", func(t *testing.T) {
		// The decision is taken on the in-memory verdict. The stored one is
		// made to differ here so that a write the decision should not have
		// made shows up; the probe-time resync is what reconciles the two.
		other := &types.Instance{
			ID: "api-1", Name: "api-1", Namespace: "default",
			ServiceID: service.ID, ServiceName: service.Name,
			Status: types.InstanceStatusRunning, CreatedAt: time.Now(),
			Ready: false, ReadyChangedAt: &changed,
		}
		require.NoError(t, testStore.CreateInstance(ctx, other))
		ih := seedHealthEntry(c, other, service)
		ih.ready, ih.promoted = true, true
		for i := 0; i < 5; i++ {
			c.updateHealthStatus(other.ID, readinessResult(true), "readiness")
		}
		time.Sleep(50 * time.Millisecond)
		var got types.Instance
		require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", other.ID, &got))
		assert.True(t, got.ReadyChangedAt.Equal(changed), "passes on an instance already ready wrote a flip")
	})

	t.Run("at the write: stored verdict already matches", func(t *testing.T) {
		require.NoError(t, c.setReady("default", inst.ID, true))
		var got types.Instance
		require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", inst.ID, &got))
		assert.True(t, got.ReadyChangedAt.Equal(changed), "ReadyChangedAt moved without a flip")
	})
}
