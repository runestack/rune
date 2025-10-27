package store

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

func TestTestStore(t *testing.T) {
	// Create a new test store
	store := NewTestStore()

	// Test context
	ctx := context.Background()

	// Test service
	service := &types.Service{
		ID:        "test-service",
		Name:      "test-service",
		Namespace: "default",
		Scale:     1,
	}

	// Test instance
	instance := &types.Instance{
		ID:        "test-instance",
		Name:      "test-instance",
		Namespace: "default",
		ServiceID: "test-service",
		Status:    types.InstanceStatusRunning,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	// Test CreateService
	err := store.CreateService(ctx, service)
	assert.NoError(t, err, "CreateService should not return an error")

	// Test GetService
	retrievedService, err := store.GetService(ctx, "default", "test-service")
	assert.NoError(t, err, "GetService should not return an error")
	assert.Equal(t, service.ID, retrievedService.ID, "Service IDs should match")
	assert.Equal(t, service.Name, retrievedService.Name, "Service names should match")

	// Test CreateInstance
	err = store.CreateInstance(ctx, instance)
	assert.NoError(t, err, "CreateInstance should not return an error")

	// Test GetInstance
	retrievedInstance, err := store.GetInstanceByID(ctx, "default", "test-instance")
	assert.NoError(t, err, "GetInstance should not return an error")
	assert.Equal(t, instance.ID, retrievedInstance.ID, "Instance IDs should match")
	assert.Equal(t, instance.ServiceID, retrievedInstance.ServiceID, "Instance ServiceIDs should match")

	// Test ListServices
	services, err := store.ListServices(ctx, "default")
	assert.NoError(t, err, "ListServices should not return an error")
	assert.Len(t, services, 1, "There should be one service")
	assert.Equal(t, "test-service", services[0].Name, "Service name should match")

	// Test ListInstances
	instances, err := store.ListInstances(ctx, "default")
	assert.NoError(t, err, "ListInstances should not return an error")
	assert.Len(t, instances, 1, "There should be one instance")
	assert.Equal(t, "test-instance", instances[0].ID, "Instance ID should match")

	// Test Update
	service.Scale = 2
	err = store.Update(ctx, types.ResourceTypeService, "default", "test-service", service)
	assert.NoError(t, err, "Update should not return an error")

	// Verify update
	retrievedService, err = store.GetService(ctx, "default", "test-service")
	assert.NoError(t, err, "GetService after update should not return an error")
	assert.Equal(t, 2, retrievedService.Scale, "Service scale should be updated to 2")

	// Test Delete
	err = store.Delete(ctx, types.ResourceTypeService, "default", "test-service")
	assert.NoError(t, err, "Delete should not return an error")

	// Verify delete
	_, err = store.GetService(ctx, "default", "test-service")
	assert.Error(t, err, "GetService after delete should return an error")

	// Test Reset
	store.Reset()

	// Verify reset
	services, err = store.ListServices(ctx, "default")
	assert.NoError(t, err, "ListServices after reset should not return an error")
	assert.Len(t, services, 0, "There should be no services after reset")
}

func TestWatch(t *testing.T) {
	store := NewTestStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up a watch
	watchCh, err := store.Watch(ctx, types.ResourceTypeService, "default")
	assert.NoError(t, err, "Watch should not return an error")

	// Add an event receiver
	events := make([]WatchEvent, 0)
	done := make(chan struct{})

	go func() {
		for event := range watchCh {
			events = append(events, event)
			if event.Type == WatchEventDeleted {
				close(done)
				return
			}
		}
	}()

	// Create a service
	service := &types.Service{
		ID:        "watched-service",
		Name:      "watched-service",
		Namespace: "default",
	}

	err = store.CreateService(ctx, service)
	assert.NoError(t, err, "CreateService should not return an error")

	// Update the service
	service.Scale = 3
	err = store.Update(ctx, types.ResourceTypeService, "default", "watched-service", service)
	assert.NoError(t, err, "Update should not return an error")

	// Delete the service
	err = store.Delete(ctx, types.ResourceTypeService, "default", "watched-service")
	assert.NoError(t, err, "Delete should not return an error")

	// Wait for all events to be received
	select {
	case <-done:
		// Continue
	case <-time.After(time.Second):
		t.Fatal("Timed out waiting for delete event")
	}

	// Check events
	assert.Len(t, events, 3, "Should have received 3 events")
	assert.Equal(t, WatchEventCreated, events[0].Type, "First event should be a create event")
	assert.Equal(t, WatchEventUpdated, events[1].Type, "Second event should be an update event")
	assert.Equal(t, WatchEventDeleted, events[2].Type, "Third event should be a delete event")
}

func TestTransaction(t *testing.T) {
	store := NewTestStore()
	ctx := context.Background()

	// Execute a transaction
	err := store.Transaction(ctx, func(tx Transaction) error {
		// Create a service in the transaction
		service := &types.Service{
			ID:        "tx-service",
			Name:      "tx-service",
			Namespace: "default",
		}

		err := tx.Create(types.ResourceTypeService, "default", "tx-service", service)
		if err != nil {
			return err
		}

		// Create an instance in the transaction
		instance := &types.Instance{
			ID:        "tx-instance",
			Name:      "tx-instance",
			Namespace: "default",
			ServiceID: "tx-service",
		}

		return tx.Create(types.ResourceTypeInstance, "default", "tx-instance", instance)
	})

	assert.NoError(t, err, "Transaction should not return an error")

	// Verify transaction results
	service, err := store.GetService(ctx, "default", "tx-service")
	assert.NoError(t, err, "GetService after transaction should not return an error")
	assert.Equal(t, "tx-service", service.Name, "Service name should match")

	instance, err := store.GetInstanceByID(ctx, "default", "tx-instance")
	assert.NoError(t, err, "GetInstance after transaction should not return an error")
	assert.Equal(t, "tx-instance", instance.Name, "Instance name should match")
}

func TestWatchScalingOperations(t *testing.T) {
	store := NewTestStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up a watch for scaling operations
	watchCh, err := store.Watch(ctx, types.ResourceTypeScalingOperation, "default")
	assert.NoError(t, err, "Watch should not return an error")

	// Add an event receiver
	events := make([]WatchEvent, 0)
	done := make(chan struct{})

	go func() {
		for event := range watchCh {
			events = append(events, event)
			if event.Type == WatchEventDeleted {
				close(done)
				return
			}
		}
	}()

	// Create a scaling operation
	op := &types.ScalingOperation{
		ID:           "test-scaling-op",
		Namespace:    "default",
		ServiceName:  "test-service",
		CurrentScale: 1,
		TargetScale:  5,
		StepSize:     2,
		Interval:     1,
		StartTime:    time.Now(),
		Status:       types.ScalingOperationStatusInProgress,
		Mode:         types.ScalingModeGradual,
	}

	err = store.Create(ctx, types.ResourceTypeScalingOperation, op.Namespace, op.ID, op)
	assert.NoError(t, err, "Create should not return an error")

	// Update the scaling operation
	op.Status = types.ScalingOperationStatusCompleted
	op.EndTime = time.Now()
	err = store.Update(ctx, types.ResourceTypeScalingOperation, op.Namespace, op.ID, op)
	assert.NoError(t, err, "Update should not return an error")

	// Delete the scaling operation
	err = store.Delete(ctx, types.ResourceTypeScalingOperation, op.Namespace, op.ID)
	assert.NoError(t, err, "Delete should not return an error")

	// Wait for all events to be received
	select {
	case <-done:
		// Continue
	case <-time.After(time.Second):
		t.Fatal("Timed out waiting for delete event")
	}

	// Check events
	assert.Len(t, events, 3, "Should have received 3 events")
	assert.Equal(t, WatchEventCreated, events[0].Type, "First event should be a create event")
	assert.Equal(t, WatchEventUpdated, events[1].Type, "Second event should be an update event")
	assert.Equal(t, WatchEventDeleted, events[2].Type, "Third event should be a delete event")

	// Check the resource in the event is a ScalingOperation
	createEvent := events[0]
	if scalingOp, ok := createEvent.Resource.(*types.ScalingOperation); ok {
		assert.Equal(t, "test-scaling-op", scalingOp.ID, "Resource ID should match")
		assert.Equal(t, types.ScalingOperationStatusInProgress, scalingOp.Status, "Initial status should be in progress")
	} else {
		t.Errorf("Expected resource to be a *types.ScalingOperation, got %T", createEvent.Resource)
	}

	updateEvent := events[1]
	if scalingOp, ok := updateEvent.Resource.(*types.ScalingOperation); ok {
		assert.Equal(t, "test-scaling-op", scalingOp.ID, "Resource ID should match")
		assert.Equal(t, types.ScalingOperationStatusCompleted, scalingOp.Status, "Updated status should be completed")
	} else {
		t.Errorf("Expected resource to be a *types.ScalingOperation, got %T", updateEvent.Resource)
	}
}
