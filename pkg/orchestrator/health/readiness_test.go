package health

import (
	"context"
	"testing"
	"time"

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

// A flapping liveness probe must pass successThreshold times in a row before
// its failures are forgiven, so alternating pass/fail still reaches the
// restart.
func TestLivenessSuccessThresholdKeepsFlappingFailures(t *testing.T) {
	baseCtx, testStore, testRunner, hc := setupHealthController(t)
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	require.NoError(t, hc.Start(ctx))
	defer hc.Stop()
	c := hc.(*healthController)

	service := &types.Service{
		ID: "flap", Name: "flap", Namespace: "default", Runtime: "container",
		RestartPolicy: types.RestartPolicyAlways,
		Health: &types.HealthCheck{Liveness: &types.Probe{
			Type: "http", Port: 80, FailureThreshold: 3, SuccessThreshold: 2,
		}},
	}
	require.NoError(t, testStore.CreateService(ctx, service))
	inst := &types.Instance{
		ID: "flap-0", Name: "flap-0", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status: types.InstanceStatusRunning, CreatedAt: time.Now(),
	}
	require.NoError(t, testStore.CreateInstance(ctx, inst))
	seedHealthEntry(c, inst, service)

	for _, ok := range []bool{false, true, false, true, false} {
		c.updateHealthStatus(inst.ID, livenessResult(ok), "liveness")
	}
	require.Eventually(t, func() bool {
		for _, id := range testRunner.GetStoppedInstances() {
			if id == inst.ID {
				return true
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond, "single passes must not reset the failure count")
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
