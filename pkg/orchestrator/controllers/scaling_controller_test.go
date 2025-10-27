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
