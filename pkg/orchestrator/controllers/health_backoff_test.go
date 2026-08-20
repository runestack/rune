package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/require"
)

// These tests cover the fixes for the probed-services restart-churn loop:
// the hardcoded liveness failure threshold, and CrashLoopBackoff state
// that reset to zero on every recreate because it was keyed on the
// per-UUID in-memory health entry while each restart mints a new UUID.

// seedHealthEntry injects a monitored instance directly (no monitor
// goroutine, no tickers) so tests drive updateHealthStatus/
// restartInstanceWithBackoff deterministically.
func seedHealthEntry(c *healthController, instance *types.Instance, service *types.Service) *instanceHealth {
	ih := &instanceHealth{instance: instance, service: service}
	c.mu.Lock()
	c.instances[instance.ID] = ih
	c.mu.Unlock()
	return ih
}

func livenessFailure() types.HealthCheckResult {
	return types.HealthCheckResult{
		Success:   false,
		Message:   "probe timed out",
		CheckTime: time.Now(),
		CheckType: "liveness",
	}
}

func TestLivenessFailureThresholdHonored(t *testing.T) {
	baseCtx, testStore, testRunner, hc := setupHealthController(t)
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	require.NoError(t, hc.Start(ctx))
	defer hc.Stop()
	c := hc.(*healthController)

	service := &types.Service{
		ID:            "threshold-svc",
		Name:          "threshold-svc",
		Namespace:     "default",
		Runtime:       "container",
		RestartPolicy: types.RestartPolicyAlways,
		Health: &types.HealthCheck{
			Liveness: &types.Probe{
				Type:             "http",
				Port:             80,
				FailureThreshold: 5, // the previously-ignored knob
			},
		},
	}
	require.NoError(t, testStore.CreateService(ctx, service))

	instance := &types.Instance{
		ID:          "threshold-instance",
		Name:        "threshold-instance",
		Namespace:   "default",
		ServiceID:   service.ID,
		ServiceName: service.Name,
		Status:      types.InstanceStatusRunning,
		CreatedAt:   time.Now(),
	}
	require.NoError(t, testStore.CreateInstance(ctx, instance))
	seedHealthEntry(c, instance, service)

	// Failures 1-4: below the configured threshold — the old hardcoded
	// >=3 would already have restarted at the third.
	for i := 0; i < 4; i++ {
		c.updateHealthStatus(instance.ID, livenessFailure(), "liveness")
	}
	require.Empty(t, testRunner.GetStoppedInstances(),
		"restarted before the configured failureThreshold was reached")

	// Failure 5 crosses the threshold → restart fires.
	//
	// The restart is dispatched asynchronously: RestartInstance now drains
	// the instance from the dataplane before stopping it, and that sleep must
	// not be held under the health controller's global mutex (it would freeze
	// health monitoring and the reconcile workers node-wide). The decision and
	// its backoff bookkeeping are still synchronous — only the teardown is
	// handed off — so this waits for the effect rather than assuming it landed
	// before the call returned.
	c.updateHealthStatus(instance.ID, livenessFailure(), "liveness")
	require.Eventually(t, func() bool {
		for _, id := range testRunner.GetStoppedInstances() {
			if id == instance.ID {
				return true
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond,
		"restart did not fire at the configured failureThreshold")
}

func TestLivenessFailureThresholdDefaultsToThree(t *testing.T) {
	require.Equal(t, 3, livenessFailureThreshold(nil))
	require.Equal(t, 3, livenessFailureThreshold(&types.Service{}))
	require.Equal(t, 3, livenessFailureThreshold(&types.Service{
		Health: &types.HealthCheck{Liveness: &types.Probe{}},
	}))
	require.Equal(t, 7, livenessFailureThreshold(&types.Service{
		Health: &types.HealthCheck{Liveness: &types.Probe{FailureThreshold: 7}},
	}))
}

// TestBackoffSeededFromPersistedRestartCount is the churn-loop regression
// test: a health-restart replacement arrives with a fresh UUID but carries
// Metadata.RestartCount; AddInstance must seed backoff from it so the loop
// can't restart at full speed forever.
func TestBackoffSeededFromPersistedRestartCount(t *testing.T) {
	_, _, _, hc := setupHealthController(t)
	c := hc.(*healthController)

	created := time.Now()
	instance := &types.Instance{
		ID:        "replacement-uuid",
		Name:      "web-0",
		Namespace: "default",
		Status:    types.InstanceStatusRunning,
		CreatedAt: created,
		Metadata:  &types.InstanceMetadata{RestartCount: 3},
	}
	// Health == nil → no monitor goroutine; seeding happens regardless.
	require.NoError(t, hc.AddInstance(&types.Service{Name: "web", Namespace: "default"}, instance))

	c.mu.RLock()
	ih := c.instances[instance.ID]
	c.mu.RUnlock()
	require.NotNil(t, ih)
	require.Equal(t, 3, ih.healthRestartCount, "backoff count not seeded from persisted RestartCount")
	require.Equal(t, created, ih.lastRestartTime, "lastRestartTime not seeded from CreatedAt")

	// A restart attempt right after creation must be deferred: count 3 →
	// 80s backoff, and the slot was created moments ago.
	err := c.restartInstanceWithBackoff(instance.ID, ih)
	require.ErrorIs(t, err, errRestartBackoff)
}

func TestBackoffNotSeededForFreshInstance(t *testing.T) {
	_, _, _, hc := setupHealthController(t)
	c := hc.(*healthController)

	instance := &types.Instance{
		ID:        "fresh-uuid",
		Name:      "web-0",
		Namespace: "default",
		Status:    types.InstanceStatusRunning,
		CreatedAt: time.Now(),
	}
	require.NoError(t, hc.AddInstance(&types.Service{Name: "web", Namespace: "default"}, instance))

	c.mu.RLock()
	ih := c.instances[instance.ID]
	c.mu.RUnlock()
	require.Zero(t, ih.healthRestartCount)
	require.True(t, ih.lastRestartTime.IsZero(), "fresh instance must start with no backoff history")
}

// TestBackoffDecaysAfterStableWindow: a slot that ran healthy past the
// reset window is a fresh incident — it restarts immediately instead of
// inheriting months of accumulated RestartCount.
func TestBackoffDecaysAfterStableWindow(t *testing.T) {
	baseCtx, testStore, testRunner, hc := setupHealthController(t)
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	require.NoError(t, hc.Start(ctx))
	defer hc.Stop()
	c := hc.(*healthController)

	service := &types.Service{
		ID:            "decay-svc",
		Name:          "decay-svc",
		Namespace:     "default",
		Runtime:       "container",
		RestartPolicy: types.RestartPolicyAlways,
	}
	require.NoError(t, testStore.CreateService(ctx, service))

	instance := &types.Instance{
		ID:          "decay-instance",
		Name:        "decay-instance",
		Namespace:   "default",
		ServiceID:   service.ID,
		ServiceName: service.Name,
		Status:      types.InstanceStatusRunning,
		CreatedAt:   time.Now().Add(-time.Hour),
	}
	require.NoError(t, testStore.CreateInstance(ctx, instance))
	ih := seedHealthEntry(c, instance, service)
	ih.healthRestartCount = 6 // long history…
	ih.lastRestartTime = time.Now().Add(-healthBackoffResetWindow - time.Minute)

	// …but the last restart was outside the reset window: history is
	// forgiven and the restart proceeds instead of waiting out 5 minutes.
	require.NoError(t, c.restartInstanceWithBackoff(instance.ID, ih))
	// Asynchronous teardown — see the note in
	// TestLivenessFailureThresholdHonored.
	require.Eventually(t, func() bool {
		for _, id := range testRunner.GetStoppedInstances() {
			if id == instance.ID {
				return true
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond)
}

// TestBackoffShiftCapped: the count is seeded from a persisted counter
// that can be large; an uncapped 10s<<count overflows to a zero/garbage
// backoff and the loop restarts at full speed again.
func TestBackoffShiftCapped(t *testing.T) {
	_, _, _, hc := setupHealthController(t)
	c := hc.(*healthController)

	instance := &types.Instance{ID: "overflow-instance", Status: types.InstanceStatusRunning}
	ih := seedHealthEntry(c, instance, nil)
	ih.healthRestartCount = 1000
	ih.lastRestartTime = time.Now().Add(-4 * time.Minute) // inside the 5m max backoff

	err := c.restartInstanceWithBackoff(instance.ID, ih)
	require.ErrorIs(t, err, errRestartBackoff,
		"shift overflow produced a zero backoff and let the restart through")
}

// TestPersistHealedContainerMapping: the runner-side heal only mutates the
// reconcile pass's in-hand copy; this write-back is what stops the health
// controller's next probe from dialing the dead container's IP.
func TestPersistHealedContainerMapping(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	testRunnerMgr := manager.NewTestRunnerManager(nil)
	ic := NewInstanceController(testStore, testRunnerMgr, log.NewLogger()).(*instanceController)

	stored := &types.Instance{
		ID:          "healed-instance",
		Name:        "web-0",
		Namespace:   "default",
		ContainerID: "stale-container-id",
		IP:          "172.17.0.3",
		Metadata:    &types.InstanceMetadata{ContainerIP: "172.17.0.3"},
	}
	require.NoError(t, testStore.CreateInstance(ctx, stored))

	healed := *stored
	healed.ContainerID = "live-container-id"
	healed.Metadata = &types.InstanceMetadata{ContainerIP: "172.17.0.9"}

	ic.persistHealedContainerMapping(ctx, &healed)

	var got types.Instance
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", stored.ID, &got))
	require.Equal(t, "live-container-id", got.ContainerID)
	require.Equal(t, "172.17.0.9", got.Metadata.ContainerIP)
	require.Equal(t, "172.17.0.9", got.IP)

	// Idempotent: a second pass (another reconcile healing the same
	// record) is a silent skip, not an error or a duplicate write.
	ic.persistHealedContainerMapping(ctx, &healed)
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", stored.ID, &got))
	require.Equal(t, "live-container-id", got.ContainerID)
}
