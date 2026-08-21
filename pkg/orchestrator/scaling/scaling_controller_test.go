package scaling

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
	controller := NewController(testStore, logger)

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

// TestImmediateScaling_BumpsGeneration is the scaling-controller half of the
// RFC #129 Phase 2 loop-suppression contract: writing a NEW desired scale must
// bump Metadata.Generation, so the reconciler treats the resulting watch event
// as a real change (not a status-only echo) and the restart scale-back-up is
// created promptly. A no-op scale (already at target) must NOT bump it.
func TestImmediateScaling_BumpsGeneration(t *testing.T) {
	testStore := store.NewTestStore()
	defer testStore.Close()
	ctx := context.Background()

	service := &types.Service{
		ID: "svc-gen", Name: "gen-service", Namespace: "default", Scale: 1,
		Metadata: &types.ServiceMetadata{
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
			Generation: 1, TemplateGeneration: 1, ObservedGeneration: 1,
		},
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeService, service.Namespace, service.Name, service))

	controller := NewController(testStore, log.NewTestLogger())
	ctxWithCancel, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, controller.Start(ctxWithCancel))
	defer controller.Stop()

	// Scale 1 → 3: Scale changes, so Generation must advance past ObservedGeneration.
	require.NoError(t, controller.CreateScalingOperation(ctx, service, types.ScalingOperationParams{
		CurrentScale: 1, TargetScale: 3, IntervalSeconds: 1, IsGradual: false,
	}))
	require.Eventually(t, func() bool {
		var s types.Service
		if err := testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &s); err != nil {
			return false
		}
		return s.Scale == 3 && noScalingOpsInProgress(t, testStore, service.Namespace)
	}, 5*time.Second, 50*time.Millisecond)

	var scaled types.Service
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &scaled))
	assert.Greater(t, scaled.Metadata.Generation, int64(1),
		"a scale change must bump Generation so it isn't skipped as status-only")
	assert.Greater(t, scaled.Metadata.Generation, scaled.Metadata.ObservedGeneration,
		"post-scale Generation must lead ObservedGeneration until the reconciler converges")
	assert.Equal(t, int64(1), scaled.Metadata.TemplateGeneration,
		"a scale change must NOT bump TemplateGeneration — that's what keeps surviving instances from being recreated (issue #142)")

	// No-op scale (3 → 3) must not bump Generation.
	genBefore := scaled.Metadata.Generation
	require.NoError(t, controller.CreateScalingOperation(ctx, &scaled, types.ScalingOperationParams{
		CurrentScale: 3, TargetScale: 3, IntervalSeconds: 1, IsGradual: false,
	}))
	require.Eventually(t, func() bool {
		return noScalingOpsInProgress(t, testStore, service.Namespace)
	}, 5*time.Second, 50*time.Millisecond)
	var afterNoop types.Service
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &afterNoop))
	assert.Equal(t, genBefore, afterNoop.Metadata.Generation,
		"a scale to the current value must not bump Generation")
}

// TestImmediateScaling_PersistsLastNonZeroScale: the scaling controller must
// persist the restart-restore hint on the record itself — a non-zero target
// records itself; a scale-to-zero remembers the pre-stop scale. (The API
// handler used to set this on a fetched copy that was never written back, so
// `rune restart` of a stopped cast-created service fell back to scale 1.)
func TestImmediateScaling_PersistsLastNonZeroScale(t *testing.T) {
	testStore := store.NewTestStore()
	defer testStore.Close()
	ctx := context.Background()

	service := &types.Service{
		ID: "svc-lnz", Name: "lnz-service", Namespace: "default", Scale: 2,
		Metadata: &types.ServiceMetadata{
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
			Generation: 1, TemplateGeneration: 1, ObservedGeneration: 1,
		},
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeService, service.Namespace, service.Name, service))

	controller := NewController(testStore, log.NewTestLogger())
	ctxWithCancel, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, controller.Start(ctxWithCancel))
	defer controller.Stop()

	scaleTo := func(current, target int) {
		var s types.Service
		require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &s))
		require.NoError(t, controller.CreateScalingOperation(ctx, &s, types.ScalingOperationParams{
			CurrentScale: current, TargetScale: target, IntervalSeconds: 1,
		}))
		require.Eventually(t, func() bool {
			var got types.Service
			if err := testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &got); err != nil {
				return false
			}
			return got.Scale == target && noScalingOpsInProgress(t, testStore, service.Namespace)
		}, 5*time.Second, 50*time.Millisecond)
	}

	// Scale up to 3: LNZ records the non-zero target.
	scaleTo(2, 3)
	var got types.Service
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &got))
	assert.Equal(t, 3, got.Metadata.LastNonZeroScale, "non-zero scale must persist as LastNonZeroScale")

	// Scale to 0 (stop): LNZ remembers the pre-stop scale.
	scaleTo(3, 0)
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &got))
	assert.Equal(t, 3, got.Metadata.LastNonZeroScale, "scale-to-zero must keep the pre-stop scale for restart restore")
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

	controller := NewController(testStore, log.NewTestLogger())
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
