package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImmediateScaling tests that a service is immediately scaled to the target scale
func TestImmediateScaling(t *testing.T) {
	// Create a test store
	testStore := store.NewTestStore()
	defer testStore.Close()

	// Create a logger
	logger := log.NewTestLogger()

	// Create a context
	ctx := context.Background()

	// Create test service with initial scale of 1
	service := &types.Service{
		ID:        "service-1",
		Name:      "test-service",
		Namespace: "default",
		Scale:     1,
		Metadata: &types.ServiceMetadata{
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
	}

	// Store the service
	err := testStore.Create(ctx, types.ResourceTypeService, service.Namespace, service.Name, service)
	require.NoError(t, err)

	// Create scaling controller
	controller := NewScalingController(testStore, logger)

	// Start the controller
	ctxWithCancel, cancel := context.WithCancel(ctx)
	defer cancel()

	err = controller.Start(ctxWithCancel)
	require.NoError(t, err)
	defer controller.Stop()

	// Create scaling params for immediate scaling to scale 5
	params := types.ScalingOperationParams{
		CurrentScale:    1,
		TargetScale:     5,
		IntervalSeconds: 1,
		IsGradual:       false, // Request immediate scaling
	}

	// Create the scaling operation
	err = controller.CreateScalingOperation(ctx, service, params)
	require.NoError(t, err)

	// check if the operation is created
	var ops []types.ScalingOperation
	err = testStore.List(ctx, types.ResourceTypeScalingOperation, service.Namespace, &ops)
	require.NoError(t, err)
	assert.Equal(t, 1, len(ops), "Operation should be created")
	assert.Equal(t, types.ScalingOperationStatusInProgress, ops[0].Status, "Operation should be in progress")

	// Wait a short time for the operation to be processed
	time.Sleep(2 * time.Second)

	// Get the service and verify it has been scaled
	var updatedService types.Service
	err = testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &updatedService)
	require.NoError(t, err)

	t.Logf("Service scale after operation: %d (target was %d)", updatedService.Scale, params.TargetScale)
	assert.Equal(t, params.TargetScale, updatedService.Scale, "Service should be scaled immediately to target")
}

// scalingTestScale reads the current desired scale of a service from the store.
func scalingTestScale(t *testing.T, st *store.TestStore, ns, name string) int {
	t.Helper()
	var s types.Service
	require.NoError(t, st.Get(context.Background(), types.ResourceTypeService, ns, name, &s))
	return s.Scale
}

// noScalingOpsInProgress reports whether every scaling operation in the
// namespace has reached a terminal status.
func noScalingOpsInProgress(t *testing.T, st *store.TestStore, ns string) bool {
	t.Helper()
	var ops []types.ScalingOperation
	require.NoError(t, st.List(context.Background(), types.ResourceTypeScalingOperation, ns, &ops))
	for i := range ops {
		if ops[i].Status == types.ScalingOperationStatusInProgress {
			return false
		}
	}
	return true
}

// TestImmediateScaling_BackToBackDownUpEndsAtUpTarget is the regression test for
// the restart flake: `rune restart` issues an immediate scale-to-0 immediately
// followed by an immediate scale-back-to-N. Both land as ScalingOperation rows
// in quick succession. When operation events were each handled in their own
// goroutine, the two operations raced their read-modify-write of service.Scale —
// and if the scale-DOWN write landed last, the service was stranded at Scale=0
// (the operator saw "1 → 0 and it hangs", and had to restart again to recover).
//
// With serial, in-creation-order processing the last-created operation always
// wins, so the desired scale must deterministically settle at the up target on
// every cycle. Looped to make the previously-racy behavior reproducible.
func TestImmediateScaling_BackToBackDownUpEndsAtUpTarget(t *testing.T) {
	testStore := store.NewTestStore()
	defer testStore.Close()
	ctx := context.Background()

	service := &types.Service{
		ID:        "svc-1",
		Name:      "web",
		Namespace: "default",
		Scale:     1,
		Metadata:  &types.ServiceMetadata{CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeService, "default", "web", service))

	controller := NewScalingController(testStore, log.NewTestLogger())
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, controller.Start(cctx))
	defer controller.Stop()

	const cycles = 50
	for i := 0; i < cycles; i++ {
		// Scale to 0 then immediately back to 1 — the restart sequence.
		cur := scalingTestScale(t, testStore, "default", "web")
		var svc types.Service
		require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, "default", "web", &svc))
		require.NoError(t, controller.CreateScalingOperation(ctx, &svc, types.ScalingOperationParams{
			CurrentScale: cur, TargetScale: 0, StepSize: 1,
		}))
		require.NoError(t, controller.CreateScalingOperation(ctx, &svc, types.ScalingOperationParams{
			CurrentScale: 0, TargetScale: 1, StepSize: 1,
		}))

		require.Eventuallyf(t, func() bool {
			return noScalingOpsInProgress(t, testStore, "default") &&
				scalingTestScale(t, testStore, "default", "web") == 1
		}, 5*time.Second, 5*time.Millisecond, "cycle %d: desired scale should settle at 1", i)
	}
}
