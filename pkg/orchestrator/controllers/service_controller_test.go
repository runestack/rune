package controllers

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestServiceController creates a service controller with test dependencies
func setupTestServiceController(t *testing.T) (context.Context, *store.TestStore, *FakeInstanceController, *FakeHealthController, ServiceController) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	testInstanceController := NewFakeInstanceController()
	mockHealthController := NewFakeHealthController()
	testLogger := log.NewLogger()

	controller, err := NewServiceController(
		testStore,
		testInstanceController,
		mockHealthController,
		testLogger,
	)
	require.NoError(t, err, "Failed to create service controller")

	return ctx, testStore, testInstanceController, mockHealthController, controller
}

// createTestService creates a test service in the store
func createTestService(ctx context.Context, t *testing.T, testStore *store.TestStore, name string) *types.Service {
	service := &types.Service{
		ID:        name,
		Name:      name,
		Namespace: "default",
		Image:     "test-image:latest",
		Command:   "test-command",
		Args:      []string{"arg1", "arg2"},
		Runtime:   "container",
		Scale:     2,
		Status:    types.ServiceStatusPending,
		Env: map[string]string{
			"ENV_VAR1": "value1",
			"ENV_VAR2": "value2",
		},
		Metadata: &types.ServiceMetadata{
			Generation: 1,
		},
	}

	err := testStore.Create(ctx, types.ResourceTypeService, service.Namespace, service.Name, service)
	require.NoError(t, err, "Failed to create test service")
	return service
}

// serviceControllerCreateTestInstance creates a test instance in the store
func serviceControllerCreateTestInstance(ctx context.Context, t *testing.T, testStore *store.TestStore, serviceName string, instanceID string, status types.InstanceStatus) *types.Instance {
	instance := &types.Instance{
		ID:          instanceID,
		Name:        instanceID,
		Namespace:   "default",
		ServiceID:   serviceName,
		ServiceName: serviceName,
		Status:      status,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	err := testStore.Create(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance)
	require.NoError(t, err, "Failed to create test instance")
	return instance
}

// TestServiceControllerLifecycle tests starting and stopping the service controller
func TestServiceControllerLifecycle(t *testing.T) {
	ctx, _, _, _, controller := setupTestServiceController(t)

	// Start the controller
	err := controller.Start(ctx)
	require.NoError(t, err, "Starting service controller should not error")

	// Stop the controller
	err = controller.Stop()
	require.NoError(t, err, "Stopping service controller should not error")
}

// TestGetServiceStatus tests the GetServiceStatus method
func TestGetServiceStatus(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Create test instances
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-1", types.InstanceStatusRunning)
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-2", types.InstanceStatusStopped)

	// Get service status
	status, err := controller.GetServiceStatus(ctx, service.Namespace, service.Name)
	require.NoError(t, err, "GetServiceStatus should not return an error")
	assert.NotNil(t, status, "Status should not be nil")
	assert.Equal(t, service.Status, status.Status, "Service status should match")
	assert.Equal(t, service.Scale, status.DesiredInstances, "Desired instances should match")
	assert.Equal(t, 1, status.RunningInstances, "Should have 1 running instance")
	assert.Equal(t, int64(0), status.ObservedGeneration, "Observed generation should be 0 initially")
}

// TestUpdateServiceStatus tests the UpdateServiceStatus method
func TestUpdateServiceStatus(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Update service status
	err := controller.UpdateServiceStatus(ctx, service, types.ServiceStatusRunning)
	require.NoError(t, err, "UpdateServiceStatus should not return an error")

	// Verify the status was updated in the store
	var updatedService types.Service
	err = testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &updatedService)
	require.NoError(t, err, "Should be able to get updated service")
	assert.Equal(t, types.ServiceStatusRunning, updatedService.Status, "Service status should be updated")
}

// TestGetServiceLogs tests the GetServiceLogs method
func TestGetServiceLogs(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Create test instances
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-1", types.InstanceStatusRunning)
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-2", types.InstanceStatusRunning)

	// Setup fake instance controller to return logs
	fakeInstanceController.GetLogsFunc = func(ctx context.Context, instance *types.Instance, opts types.LogOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("logs from " + instance.ID)), nil
	}

	// Get service logs
	logs, err := controller.GetServiceLogs(ctx, service.Namespace, service.Name, types.LogOptions{
		ShowLogs: true,
	})
	require.NoError(t, err, "GetServiceLogs should not return an error")
	assert.NotNil(t, logs, "Logs should not be nil")

	// Read and verify logs
	logData, err := io.ReadAll(logs)
	require.NoError(t, err, "Should be able to read logs")
	logs.Close()

	logContent := string(logData)
	assert.Contains(t, logContent, "logs from instance-1", "Should contain logs from first instance")
	assert.Contains(t, logContent, "logs from instance-2", "Should contain logs from second instance")
}

// TestGetServiceLogsNoInstances tests GetServiceLogs when no instances exist
func TestGetServiceLogsNoInstances(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)

	// Create a test service without instances
	service := createTestService(ctx, t, testStore, "test-service")

	// Get service logs
	logs, err := controller.GetServiceLogs(ctx, service.Namespace, service.Name, types.LogOptions{
		ShowLogs: true,
	})
	require.Error(t, err, "GetServiceLogs should return an error when no instances exist")
	assert.Nil(t, logs, "Logs should be nil when no instances exist")
	assert.Contains(t, err.Error(), "no instances found", "Error should mention no instances found")
}

// TestExecInService tests the ExecInService method
func TestExecInService(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Create test instances
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-1", types.InstanceStatusRunning)
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-2", types.InstanceStatusStopped)

	// Setup fake instance controller to return exec stream
	fakeInstanceController.ExecFunc = func(ctx context.Context, instance *types.Instance, options types.ExecOptions) (types.ExecStream, error) {
		return &fakeExecStream{
			stdout:   []byte("exec output"),
			stderr:   []byte("exec error"),
			exitCode: 0,
		}, nil
	}

	// Execute command in service
	execStream, err := controller.ExecInService(ctx, service.Namespace, service.Name, types.ExecOptions{
		Command: []string{"ls", "-la"},
	})
	require.NoError(t, err, "ExecInService should not return an error")
	assert.NotNil(t, execStream, "Exec stream should not be nil")

	// Verify exec was called on a running instance
	assert.Len(t, fakeInstanceController.ExecCalls, 1, "Exec should be called once")
	assert.Equal(t, "instance-1", fakeInstanceController.ExecCalls[0].Instance.ID, "Should exec on running instance")
}

// TestExecInServiceNoRunningInstances tests ExecInService when no running instances exist
func TestExecInServiceNoRunningInstances(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Create only stopped instances
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-1", types.InstanceStatusStopped)

	// Execute command in service
	execStream, err := controller.ExecInService(ctx, service.Namespace, service.Name, types.ExecOptions{
		Command: []string{"ls", "-la"},
	})
	require.Error(t, err, "ExecInService should return an error when no running instances exist")
	assert.Nil(t, execStream, "Exec stream should be nil")
	assert.Contains(t, err.Error(), "no running instances found", "Error should mention no running instances")
}

// TestRestartService_StampsTemplateGeneration: restart is one atomic template
// restamp (issue #140) — Generation++ and TemplateGeneration = Generation, so
// the reconciler replaces every instance; the desired scale is untouched.
func TestRestartService_StampsTemplateGeneration(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)

	service := createTestService(ctx, t, testStore, "test-service") // Gen 1, Scale 2

	templateGen, scale, err := controller.RestartService(ctx, service.Namespace, service.Name)
	require.NoError(t, err, "RestartService should not return an error")

	var got types.Service
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &got))
	assert.Equal(t, int64(2), got.Metadata.Generation, "restart must bump Generation")
	assert.Equal(t, int64(2), got.Metadata.TemplateGeneration, "restart must stamp TemplateGeneration = Generation")
	assert.Equal(t, got.Metadata.TemplateGeneration, templateGen, "returned templateGeneration must match the stamp")
	assert.Equal(t, 2, got.Scale, "restart must NOT change the desired scale")
	assert.Equal(t, 2, scale, "returned scale must be the converge target")
}

// TestRestartService_StoppedServiceStarts: restarting a stopped service
// (scale 0) starts it at its last non-zero scale, falling back to 1.
func TestRestartService_StoppedServiceStarts(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)

	stopped := &types.Service{
		ID: "stopped-svc", Name: "stopped-svc", Namespace: "default",
		Image: "test:latest", Runtime: "container", Scale: 0,
		Metadata: &types.ServiceMetadata{Generation: 4, TemplateGeneration: 2, LastNonZeroScale: 3},
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeService, stopped.Namespace, stopped.Name, stopped))

	_, scale, err := controller.RestartService(ctx, stopped.Namespace, stopped.Name)
	require.NoError(t, err)
	assert.Equal(t, 3, scale, "stopped service must restart at LastNonZeroScale")

	var got types.Service
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, stopped.Namespace, stopped.Name, &got))
	assert.Equal(t, 3, got.Scale)
	assert.Equal(t, got.Metadata.Generation, got.Metadata.TemplateGeneration, "restart stamps template = generation")

	// No LastNonZeroScale recorded → fall back to 1.
	bare := &types.Service{
		ID: "bare-svc", Name: "bare-svc", Namespace: "default",
		Image: "test:latest", Runtime: "container", Scale: 0,
		Metadata: &types.ServiceMetadata{Generation: 1},
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeService, bare.Namespace, bare.Name, bare))
	_, scale, err = controller.RestartService(ctx, bare.Namespace, bare.Name)
	require.NoError(t, err)
	assert.Equal(t, 1, scale, "no LastNonZeroScale → start one instance")
}

// TestStopService tests the StopService method
func TestStopService(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Create test instances
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-1", types.InstanceStatusRunning)
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-2", types.InstanceStatusRunning)

	// Stop service
	err := controller.StopService(ctx, service.Namespace, service.Name)
	require.NoError(t, err, "StopService should not return an error")

	// Verify stop was called on all instances
	assert.Len(t, fakeInstanceController.StopInstanceCalls, 2, "Stop should be called on both instances")
}

// TestDeleteService_TombstonesRecord verifies the RFC #129 Phase 4 foreground
// deletion contract: DeleteService STAMPS a tombstone (DeletionTimestamp +
// Finalizers) on the service record instead of removing it or spinning up an
// async task; a re-issue is an idempotent no-op success.
func TestDeleteService_TombstonesRecord(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)
	service := createTestService(ctx, t, testStore, "svc") // scale 2, no claimTemplate

	resp, err := controller.DeleteService(ctx, &types.DeletionRequest{
		Namespace: service.Namespace, Name: service.Name,
	})
	require.NoError(t, err)
	assert.Equal(t, "in_progress", resp.Status)

	// The record must still exist, now tombstoned — NOT removed.
	var got types.Service
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &got))
	require.NotNil(t, got.Metadata.DeletionTimestamp, "DeleteService must tombstone, not remove the record")
	assert.Equal(t, []types.FinalizerType{types.FinalizerTypeInstanceCleanup}, got.Metadata.Finalizers,
		"no claimTemplate volumes → only instance-cleanup is a finalizer (ServiceDeregister is terminal, not a finalizer)")
	assert.Equal(t, types.ServiceStatusDeleted, got.Status)

	// Re-issuing delete on a tombstoned service is a no-op success, and must
	// not re-stamp the timestamp.
	firstTS := got.Metadata.DeletionTimestamp.UnixNano()
	resp2, err := controller.DeleteService(ctx, &types.DeletionRequest{
		Namespace: service.Namespace, Name: service.Name,
	})
	require.NoError(t, err, "re-issuing delete on a tombstoned service must be idempotent success")
	assert.Equal(t, "in_progress", resp2.Status)

	var got2 types.Service
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &got2))
	assert.Equal(t, firstTS, got2.Metadata.DeletionTimestamp.UnixNano(), "re-issue must not re-stamp the tombstone")
}

// TestListInstancesForService tests the ListInstancesForService method
func TestListInstancesForService(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Create test instances for this service
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-1", types.InstanceStatusRunning)
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-2", types.InstanceStatusStopped)

	// Create an instance for a different service
	serviceControllerCreateTestInstance(ctx, t, testStore, "other-service", "other-instance", types.InstanceStatusRunning)

	// List instances for the service
	instances, err := controller.listInstancesForService(ctx, service.Namespace, service.Name)
	require.NoError(t, err, "ListInstancesForService should not return an error")
	assert.Len(t, instances, 2, "Should return 2 instances for the service")

	// Verify the instances belong to the correct service
	instanceIDs := make(map[string]bool)
	for _, instance := range instances {
		instanceIDs[instance.ID] = true
		assert.Equal(t, service.Name, instance.ServiceName, "Instance should belong to the correct service")
	}

	assert.True(t, instanceIDs["instance-1"], "Should include instance-1")
	assert.True(t, instanceIDs["instance-2"], "Should include instance-2")
}

// TestDeleteServiceDryRun tests the DeleteService method with dry run
func TestDeleteServiceDryRun(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Create test instances
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-1", types.InstanceStatusRunning)

	// Delete service with dry run
	request := &types.DeletionRequest{
		Namespace: service.Namespace,
		Name:      service.Name,
		DryRun:    true,
	}

	response, err := controller.DeleteService(ctx, request)
	require.NoError(t, err, "DeleteService should not return an error")
	assert.NotNil(t, response, "Response should not be nil")
	assert.Equal(t, "dry_run", response.Status, "Status should be dry_run")
	assert.Equal(t, "dry-run", response.DeletionID, "Deletion ID should be dry-run")
	assert.Len(t, response.Finalizers, 2, "Should have 2 finalizers")
}

// TestDeleteServiceReal tests the DeleteService method with real deletion
func TestDeleteServiceReal(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Create test instances
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-1", types.InstanceStatusRunning)

	// Delete service
	request := &types.DeletionRequest{
		Namespace: service.Namespace,
		Name:      service.Name,
		DryRun:    false,
	}

	response, err := controller.DeleteService(ctx, request)
	require.NoError(t, err, "DeleteService should not return an error")
	assert.NotNil(t, response, "Response should not be nil")
	assert.Equal(t, "in_progress", response.Status, "Status should be in_progress")
	assert.NotEmpty(t, response.DeletionID, "Deletion ID should not be empty")
	assert.Len(t, response.Finalizers, 2, "Should have 2 finalizers")

	// Verify deletion operation was created in store
	var deletionOp types.DeletionOperation
	err = testStore.Get(ctx, types.ResourceTypeDeletionOperation, service.Namespace, response.DeletionID, &deletionOp)
	require.NoError(t, err, "Deletion operation should be stored")
	assert.Equal(t, service.Name, deletionOp.ServiceName, "Deletion operation should reference the service")
}

// TestGetDeletionStatus tests the GetDeletionStatus method
func TestGetDeletionStatus(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)

	// Create a deletion operation
	deletionOp := &types.DeletionOperation{
		ID:               "test-deletion",
		Namespace:        "default",
		ServiceName:      "test-service",
		Status:           types.DeletionOperationStatusInitializing,
		TotalInstances:   2,
		DeletedInstances: 1,
		FailedInstances:  0,
		StartTime:        time.Now(),
	}

	err := testStore.Create(ctx, types.ResourceTypeDeletionOperation, deletionOp.Namespace, deletionOp.ID, deletionOp)
	require.NoError(t, err, "Failed to create deletion operation")

	// Get deletion status
	status, err := controller.GetDeletionStatus(ctx, deletionOp.Namespace, deletionOp.ID)
	require.NoError(t, err, "GetDeletionStatus should not return an error")
	assert.NotNil(t, status, "Status should not be nil")
	assert.Equal(t, deletionOp.ID, status.ID, "Deletion ID should match")
	assert.Equal(t, deletionOp.Status, status.Status, "Status should match")
	assert.Equal(t, deletionOp.TotalInstances, status.TotalInstances, "Total instances should match")
	assert.Equal(t, deletionOp.DeletedInstances, status.DeletedInstances, "Deleted instances should match")
}

// TestHandleServiceCreated tests the HandleServiceCreated method
// TestServiceEvent_EnqueuesKey verifies the watch→queue translation (RFC #129
// Phase 3): Created, real Updated, and Deleted events all enqueue the service
// key; duplicates coalesce.
func TestServiceEvent_EnqueuesKey(t *testing.T) {
	_, _, _, _, controller := setupTestServiceController(t)
	sc := controller.(*serviceController)

	created := store.WatchEvent{
		Type:         store.WatchEventCreated,
		ResourceType: types.ResourceTypeService,
		Namespace:    "default",
		Name:         "web",
	}
	sc.enqueueServiceEvent(created)
	assert.Equal(t, 1, sc.reconciler.queue.Len(), "created event must enqueue")

	// A real desired-state update (generation ahead of observed) enqueues —
	// and coalesces with the pending created key.
	updated := created
	updated.Type = store.WatchEventUpdated
	updated.Resource = &types.Service{
		Name: "web", Namespace: "default",
		Metadata: &types.ServiceMetadata{Generation: 2, ObservedGeneration: 1},
	}
	sc.enqueueServiceEvent(updated)
	assert.Equal(t, 1, sc.reconciler.queue.Len(), "same key must coalesce, not duplicate")

	// A different service's deleted event enqueues its own key (syncService
	// treats not-found as settled).
	deleted := store.WatchEvent{
		Type:         store.WatchEventDeleted,
		ResourceType: types.ResourceTypeService,
		Namespace:    "default",
		Name:         "gone",
	}
	sc.enqueueServiceEvent(deleted)
	assert.Equal(t, 2, sc.reconciler.queue.Len(), "deleted event must enqueue its own key")
}

// TestStatusOnlyUpdate_SkipsEnqueue verifies the echo filter: an Updated event
// whose resource shows ObservedGeneration == Generation is the reconciler's
// own status write bouncing back through the watch, and must not enqueue.
// Anything not inspectable must enqueue (never guess toward dropping work).
func TestStatusOnlyUpdate_SkipsEnqueue(t *testing.T) {
	_, _, _, _, controller := setupTestServiceController(t)
	sc := controller.(*serviceController)

	converged := &types.ServiceMetadata{Generation: 3, ObservedGeneration: 3}

	// Pointer form: filtered.
	sc.enqueueServiceEvent(store.WatchEvent{
		Type: store.WatchEventUpdated, ResourceType: types.ResourceTypeService,
		Namespace: "default", Name: "web",
		Resource: &types.Service{Name: "web", Namespace: "default", Metadata: converged},
	})
	assert.Equal(t, 0, sc.reconciler.queue.Len(), "status-only echo (pointer) must not enqueue")

	// Value form: filtered too.
	sc.enqueueServiceEvent(store.WatchEvent{
		Type: store.WatchEventUpdated, ResourceType: types.ResourceTypeService,
		Namespace: "default", Name: "web",
		Resource: types.Service{Name: "web", Namespace: "default", Metadata: converged},
	})
	assert.Equal(t, 0, sc.reconciler.queue.Len(), "status-only echo (value) must not enqueue")

	// Uninspectable resource: must enqueue.
	sc.enqueueServiceEvent(store.WatchEvent{
		Type: store.WatchEventUpdated, ResourceType: types.ResourceTypeService,
		Namespace: "default", Name: "web",
		Resource: map[string]any{"opaque": true},
	})
	assert.Equal(t, 1, sc.reconciler.queue.Len(), "uninspectable update must enqueue")
}

// TestSyncService_NotFoundIsTerminal: a key whose service no longer exists is
// settled (nil), not retried — deletion cleanup belongs to finalizers and
// housekeeping.
func TestSyncService_NotFoundIsTerminal(t *testing.T) {
	ctx, _, _, _, controller := setupTestServiceController(t)
	sc := controller.(*serviceController)

	err := sc.reconciler.syncService(ctx, "default/ghost")
	assert.NoError(t, err, "missing service must be terminal, not a requeue")
}

// TestSyncService_MalformedKeyDropped: a key without a namespace separator can
// never succeed; it must be dropped (nil) rather than retried forever.
func TestSyncService_MalformedKeyDropped(t *testing.T) {
	ctx, _, _, _, controller := setupTestServiceController(t)
	sc := controller.(*serviceController)

	err := sc.reconciler.syncService(ctx, "no-separator")
	assert.NoError(t, err, "malformed key must be dropped, not requeued")
}

// TestResyncEnqueuesAll: the periodic resync schedules every service in the
// store — the level-triggered safety net behind the event-driven flow.
func TestResyncEnqueuesAll(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)
	sc := controller.(*serviceController)

	createTestService(ctx, t, testStore, "svc-a")
	createTestService(ctx, t, testStore, "svc-b")
	createTestService(ctx, t, testStore, "svc-c")

	require.NoError(t, sc.reconciler.enqueueAllServices(ctx))
	assert.Equal(t, 3, sc.reconciler.queue.Len(), "every service must be enqueued on resync")
}

// TestController_EventDrivenReconcileConverges is the end-to-end path through
// the new machinery: store create → watch event → enqueue → queue worker →
// reconcileService → instances created → status converges to Running with
// ObservedGeneration == Generation. Replaces the coverage of the deleted
// direct-call handler tests.
func TestController_EventDrivenReconcileConverges(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, controller := setupTestServiceController(t)

	// The fake persists created instances as Running, emulating the real
	// instance controller + an instantly-healthy workload.
	fakeInstanceController.CreateInstanceFunc = func(ctx context.Context, svc *types.Service, instanceName string, ordinal int) (*types.Instance, error) {
		inst := &types.Instance{
			ID: instanceName, Name: instanceName,
			Namespace: svc.Namespace, ServiceID: svc.ID, ServiceName: svc.Name,
			Status:    types.InstanceStatusRunning,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		_ = testStore.Create(ctx, types.ResourceTypeInstance, svc.Namespace, inst.ID, inst)
		return inst, nil
	}

	require.NoError(t, controller.Start(ctx))
	defer func() { _ = controller.Stop() }()

	// Creating the service fires the watch → enqueue → worker pipeline.
	service := createTestService(ctx, t, testStore, "converge-service")

	require.Eventually(t, func() bool {
		var got types.Service
		if err := testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &got); err != nil {
			return false
		}
		var instances []types.Instance
		if err := testStore.List(ctx, types.ResourceTypeInstance, service.Namespace, &instances); err != nil {
			return false
		}
		return got.Status == types.ServiceStatusRunning &&
			len(instances) == service.Scale &&
			got.Metadata != nil &&
			got.Metadata.ObservedGeneration == got.Metadata.Generation
	}, 10*time.Second, 20*time.Millisecond,
		"service must converge to Running at scale with ObservedGeneration recorded, driven by events (no manual reconcile call)")
}

// TestInstanceEvent_EnqueuesOwner: an instance event maps to its owning
// service's key (event-driven status roll-up, RFC #129 Phase 3c).
func TestInstanceEvent_EnqueuesOwner(t *testing.T) {
	_, _, _, _, controller := setupTestServiceController(t)
	sc := controller.(*serviceController)

	sc.enqueueInstanceEvent(store.WatchEvent{
		Type:         store.WatchEventUpdated,
		ResourceType: types.ResourceTypeInstance,
		Namespace:    "default",
		Name:         "web-abc12",
		Source:       store.EventSourceHealthController,
		Resource: &types.Instance{
			ID: "web-abc12", Name: "web-abc12",
			Namespace: "default", ServiceName: "web",
			Status: types.InstanceStatusRunning,
		},
	})
	assert.Equal(t, 1, sc.reconciler.queue.Len(), "instance event must enqueue its owner")

	// Value form maps too, and coalesces with the pending key.
	sc.enqueueInstanceEvent(store.WatchEvent{
		Type:         store.WatchEventUpdated,
		ResourceType: types.ResourceTypeInstance,
		Namespace:    "default",
		Name:         "web-def34",
		Resource: types.Instance{
			ID: "web-def34", Namespace: "default", ServiceName: "web",
		},
	})
	assert.Equal(t, 1, sc.reconciler.queue.Len(), "same owner must coalesce")

	// An instance without an owner is dropped.
	sc.enqueueInstanceEvent(store.WatchEvent{
		Type:         store.WatchEventUpdated,
		ResourceType: types.ResourceTypeInstance,
		Namespace:    "default",
		Name:         "orphan-xyz",
		Resource:     &types.Instance{ID: "orphan-xyz", Namespace: "default"},
	})
	assert.Equal(t, 1, sc.reconciler.queue.Len(), "ownerless instance must not enqueue")
}

// TestInstanceEvent_ReconcilerSourceSuppressed: the reconciler's own instance
// writes (UpdateInstance during a sync) must not re-enqueue their owner — that
// run already ends with updateServiceStatus, so the event is a pure echo.
func TestInstanceEvent_ReconcilerSourceSuppressed(t *testing.T) {
	_, _, _, _, controller := setupTestServiceController(t)
	sc := controller.(*serviceController)

	sc.enqueueInstanceEvent(store.WatchEvent{
		Type:         store.WatchEventUpdated,
		ResourceType: types.ResourceTypeInstance,
		Namespace:    "default",
		Name:         "web-abc12",
		Source:       store.EventSourceReconciler,
		Resource: &types.Instance{
			ID: "web-abc12", Namespace: "default", ServiceName: "web",
		},
	})
	assert.Equal(t, 0, sc.reconciler.queue.Len(), "reconciler-sourced instance events are echoes and must be dropped")
}

// TestInstanceEvent_FallsBackToStoreLookup: an event without an inspectable
// resource is resolved by reading the instance from the store; if the instance
// is gone too, the event is dropped (resync covers it).
func TestInstanceEvent_FallsBackToStoreLookup(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)
	sc := controller.(*serviceController)

	serviceControllerCreateTestInstance(ctx, t, testStore, "web", "web-abc12", types.InstanceStatusRunning)

	// No Resource on the event → store lookup maps the owner.
	sc.enqueueInstanceEvent(store.WatchEvent{
		Type:         store.WatchEventUpdated,
		ResourceType: types.ResourceTypeInstance,
		Namespace:    "default",
		Name:         "web-abc12",
	})
	assert.Equal(t, 1, sc.reconciler.queue.Len(), "store lookup must resolve the owner")

	// Unmappable: no resource, not in store → dropped.
	sc.enqueueInstanceEvent(store.WatchEvent{
		Type:         store.WatchEventDeleted,
		ResourceType: types.ResourceTypeInstance,
		Namespace:    "default",
		Name:         "long-gone",
	})
	assert.Equal(t, 1, sc.reconciler.queue.Len(), "unmappable deleted instance must be dropped")
}

// TestInstanceChange_RollsUpStatusEventDriven is the headline Phase 3c test:
// with the periodic resync stretched far out of reach, an instance's
// Starting→Running transition (a health-controller-sourced write) must roll up
// to service Status=Running within a couple of seconds — proving the roll-up
// is event-driven, not tick-driven.
func TestInstanceChange_RollsUpStatusEventDriven(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, controller := setupTestServiceController(t)
	sc := controller.(*serviceController)
	sc.reconciler.reconcileInterval = 60 * time.Second // resync can't help us

	// Created instances start as Starting (not yet ready).
	fakeInstanceController.CreateInstanceFunc = func(ctx context.Context, svc *types.Service, instanceName string, ordinal int) (*types.Instance, error) {
		inst := &types.Instance{
			ID: instanceName, Name: instanceName,
			Namespace: svc.Namespace, ServiceID: svc.ID, ServiceName: svc.Name,
			Status:    types.InstanceStatusStarting,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		_ = testStore.Create(ctx, types.ResourceTypeInstance, svc.Namespace, inst.ID, inst)
		return inst, nil
	}

	require.NoError(t, controller.Start(ctx))
	defer func() { _ = controller.Stop() }()

	service := &types.Service{
		ID: "rollup-service", Name: "rollup-service", Namespace: "default",
		Image: "test-image:latest", Runtime: "container", Scale: 1,
		Metadata: &types.ServiceMetadata{Generation: 1},
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeService, service.Namespace, service.Name, service))

	// Wait for the instance to exist (created Starting by the event-driven reconcile).
	var instanceID string
	require.Eventually(t, func() bool {
		var instances []types.Instance
		if err := testStore.List(ctx, types.ResourceTypeInstance, service.Namespace, &instances); err != nil {
			return false
		}
		for i := range instances {
			if instances[i].ServiceName == service.Name {
				instanceID = instances[i].ID
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "instance must be created by the event-driven reconcile")

	// Emulate the health controller's readiness promotion: Starting→Running
	// via a health-controller-sourced UpdateFunc — exactly the write shape
	// promoteToRunningOnReady performs.
	var inst types.Instance
	require.NoError(t, testStore.UpdateFunc(ctx, types.ResourceTypeInstance, service.Namespace, instanceID, &inst, func() error {
		inst.Status = types.InstanceStatusRunning
		return nil
	}, store.WithHealthController()))
	promotedAt := time.Now()

	// Service status must flip to Running fast — event-driven, not the 60s tick.
	require.Eventually(t, func() bool {
		var got types.Service
		if err := testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &got); err != nil {
			return false
		}
		return got.Status == types.ServiceStatusRunning
	}, 2*time.Second, 10*time.Millisecond,
		"service status must roll up within ~2s of instance readiness (event-driven)")
	t.Logf("status rolled up in %s", time.Since(promotedAt))
}

// TestRestartService_ReplacesAllInstances is the end-to-end path for issue
// #140: with the controller running, a restart stamp must cause the reconciler
// to replace every instance (new IDs, current template generation) while the
// desired scale never changes.
func TestRestartService_ReplacesAllInstances(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, controller := setupTestServiceController(t)

	// Fake instance controller persists Running instances recording the
	// service's CURRENT TemplateGeneration (matching the real controller),
	// and deletion removes the record so replacement converges.
	fakeInstanceController.CreateInstanceFunc = func(ctx context.Context, svc *types.Service, instanceName string, ordinal int) (*types.Instance, error) {
		var tgen int64
		if svc.Metadata != nil {
			tgen = svc.Metadata.TemplateGeneration
		}
		inst := &types.Instance{
			ID: instanceName, Name: instanceName,
			Namespace: svc.Namespace, ServiceID: svc.ID, ServiceName: svc.Name,
			Status:    types.InstanceStatusRunning,
			Metadata:  &types.InstanceMetadata{ServiceGeneration: tgen},
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		_ = testStore.Create(ctx, types.ResourceTypeInstance, svc.Namespace, inst.ID, inst)
		return inst, nil
	}
	fakeInstanceController.DeleteInstanceFunc = func(ctx context.Context, instance *types.Instance) error {
		return testStore.Delete(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID)
	}
	// Mirror the real controller's template compatibility rule so the restart
	// stamp actually drives replacement (the fake defaults to always-compatible).
	fakeInstanceController.IsCompatibleFunc = func(ctx context.Context, instance *types.Instance, service *types.Service) (bool, string) {
		var instGen, tmplGen int64
		if instance.Metadata != nil {
			instGen = instance.Metadata.ServiceGeneration
		}
		if service.Metadata != nil {
			tmplGen = service.Metadata.TemplateGeneration
		}
		if instGen < tmplGen {
			return false, fmt.Sprintf("service template changed: %d -> %d", instGen, tmplGen)
		}
		return true, ""
	}

	require.NoError(t, controller.Start(ctx))
	defer func() { _ = controller.Stop() }()

	service := createTestService(ctx, t, testStore, "restart-e2e") // Scale 2

	listLive := func() map[string]int64 {
		out := map[string]int64{}
		var instances []types.Instance
		_ = testStore.List(ctx, types.ResourceTypeInstance, service.Namespace, &instances)
		for i := range instances {
			if instances[i].ServiceName != service.Name || instances[i].Status == types.InstanceStatusDeleted {
				continue
			}
			var gen int64
			if instances[i].Metadata != nil {
				gen = instances[i].Metadata.ServiceGeneration
			}
			out[instances[i].ID] = gen
		}
		return out
	}

	// Converge initial deployment.
	require.Eventually(t, func() bool { return len(listLive()) == 2 }, 10*time.Second, 20*time.Millisecond)
	before := listLive()

	// Restart: one stamp, reconciler does the rest.
	templateGen, scale, err := controller.(*serviceController).RestartService(ctx, service.Namespace, service.Name)
	require.NoError(t, err)
	assert.Equal(t, 2, scale, "restart must keep the desired scale")

	require.Eventually(t, func() bool {
		live := listLive()
		if len(live) != 2 {
			return false
		}
		for id, gen := range live {
			if _, existedBefore := before[id]; existedBefore {
				return false // old instance still present
			}
			if gen < templateGen {
				return false // replacement predates the stamp
			}
		}
		return true
	}, 10*time.Second, 20*time.Millisecond,
		"every instance must be replaced with a fresh one at the stamped template generation")

	// Desired scale untouched end-to-end.
	var got types.Service
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &got))
	assert.Equal(t, 2, got.Scale, "scale must never have been part of the restart")
}

// fakeExecStream implements types.ExecStream for testing
type fakeExecStream struct {
	stdout   []byte
	stderr   []byte
	exitCode int
	closed   bool
}

func (fes *fakeExecStream) Read(p []byte) (n int, err error) {
	if fes.closed {
		return 0, io.EOF
	}
	if len(fes.stdout) == 0 {
		return 0, io.EOF
	}
	n = copy(p, fes.stdout)
	fes.stdout = fes.stdout[n:]
	return n, nil
}

func (fes *fakeExecStream) Write(p []byte) (n int, err error) {
	return len(p), nil
}

func (fes *fakeExecStream) Close() error {
	fes.closed = true
	return nil
}

func (fes *fakeExecStream) ExitCode() (int, error) {
	return fes.exitCode, nil
}

func (fes *fakeExecStream) ResizeTerminal(width, height uint32) error {
	return nil
}

func (fes *fakeExecStream) Signal(signal string) error {
	return nil
}

func (fes *fakeExecStream) Stderr() io.Reader {
	return strings.NewReader(string(fes.stderr))
}

// TestGetServiceLogs_FallsBackToFailedTombstoneSnapshot is the
// service-level regression guard for bug
// RUNE-BUG-RUNE-LOGS-IGNORES-TOMBSTONE-LASTLOGS: when a service has
// no Running instances, `rune logs <service>` should fall back to the
// most-recent Failed tombstone's LastLogs instead of returning
// "no instances found" / "all marked as deleted". Operators
// investigating a crashed service hit this all the time.
func TestGetServiceLogs_FallsBackToFailedTombstoneSnapshot(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, controller := setupTestServiceController(t)
	service := createTestService(ctx, t, testStore, "test-service")

	// Seed two Failed tombstones with snapshots, plus one Deleted (older).
	older := time.Now().Add(-1 * time.Hour)
	newer := time.Now().Add(-1 * time.Minute)
	tombs := []*types.Instance{
		{ID: "deleted-old", Name: "tomb-0", Namespace: "default", ServiceID: service.ID, ServiceName: service.Name,
			Status: types.InstanceStatusDeleted, FailedAt: &older, LastLogs: []byte("deleted-old-logs")},
		{ID: "failed-older", Name: "tomb-0", Namespace: "default", ServiceID: service.ID, ServiceName: service.Name,
			Status: types.InstanceStatusFailed, FailedAt: &older, LastLogs: []byte("failed-older-logs")},
		{ID: "failed-newer", Name: "tomb-0", Namespace: "default", ServiceID: service.ID, ServiceName: service.Name,
			Status: types.InstanceStatusFailed, FailedAt: &newer, LastLogs: []byte("failed-newer-logs")},
	}
	for _, t2 := range tombs {
		require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", t2.ID, t2))
	}
	// Stub the instance controller to surface LastLogs.
	fakeInstanceController.GetLogsFunc = func(ctx context.Context, instance *types.Instance, _ types.LogOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(instance.LastLogs))), nil
	}

	rc, err := controller.GetServiceLogs(ctx, service.Namespace, service.Name, types.LogOptions{ShowLogs: true})
	require.NoError(t, err, "fallback must succeed when a tombstone has LastLogs")
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	rc.Close()
	assert.Contains(t, string(body), "failed-newer-logs",
		"fallback must pick the most-recent Failed tombstone by FailedAt")
	assert.NotContains(t, string(body), "failed-older-logs",
		"older Failed tombstone must not bleed into the fallback")
	assert.NotContains(t, string(body), "deleted-old-logs",
		"Failed tombstones must be preferred over Deleted ones")
}

// TestGetServiceLogs_FallsBackToDeletedWhenNoFailed covers the symmetric
// fallback: no Failed tombstones, but a Deleted record still carries a
// LastLogs snapshot. The fallback must use it rather than returning
// "no logs available".
func TestGetServiceLogs_FallsBackToDeletedWhenNoFailed(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, controller := setupTestServiceController(t)
	service := createTestService(ctx, t, testStore, "test-service")

	at := time.Now().Add(-5 * time.Minute)
	del := &types.Instance{
		ID: "deleted-with-logs", Name: "tomb-0", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status: types.InstanceStatusDeleted, FailedAt: &at,
		LastLogs: []byte("deleted-tombstone-says-hello"),
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", del.ID, del))
	fakeInstanceController.GetLogsFunc = func(ctx context.Context, instance *types.Instance, _ types.LogOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(instance.LastLogs))), nil
	}

	rc, err := controller.GetServiceLogs(ctx, service.Namespace, service.Name, types.LogOptions{ShowLogs: true})
	require.NoError(t, err)
	body, _ := io.ReadAll(rc)
	rc.Close()
	assert.Contains(t, string(body), "deleted-tombstone-says-hello")
}

// TestGetServiceLogs_NoCapturedOutput_SynthesizesWhyLine asserts the
// service-level counterpart of the
// TestGetInstanceLogs_TerminalNoLogs_SynthesizesWhyLine guarantee:
// when no live instance has logs AND no tombstone has a snapshot,
// the most-recent terminal tombstone's FailureReason / StatusMessage
// is surfaced through `rune logs <service>` as a synthesized line.
// Without this, `rune logs gateway` on a crashing-without-stdout
// service silently returns nothing and operators have no signal.
func TestGetServiceLogs_NoCapturedOutput_SynthesizesWhyLine(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, controller := setupTestServiceController(t)
	service := createTestService(ctx, t, testStore, "test-service")
	at := time.Now()
	tomb := &types.Instance{
		ID: "no-snapshot", Name: "tomb-0", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status: types.InstanceStatusFailed, FailedAt: &at,
		FailureReason: "HealthCheckFailure",
		StatusMessage: "container exited with code 137 before binding port",
		// LastLogs intentionally empty.
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", tomb.ID, tomb))
	// Stub the per-instance lookup so the service-level fallback
	// can reach the synthesised-line path. The fake's default
	// returns nil, nil — matching the real GetInstanceLogs return
	// when an instance has no logs and no container — so a
	// non-nil stub is required to exercise the synth path.
	fakeInstanceController.GetLogsFunc = func(ctx context.Context, instance *types.Instance, _ types.LogOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(synthesizeNoLogsLine(instance))), nil
	}

	rc, err := controller.GetServiceLogs(ctx, service.Namespace, service.Name, types.LogOptions{ShowLogs: true})
	require.NoError(t, err)
	body, _ := io.ReadAll(rc)
	rc.Close()
	got := string(body)
	assert.Contains(t, got, "HealthCheckFailure")
	assert.Contains(t, got, "no captured output")
}

// TestGetServiceLogs_SilentLiveInstance_FallsBackToTombstone is the
// regression guard for the prod/gateway live observation: a Running
// instance whose container is genuinely silent (docker logs = 0
// bytes — the bun process started but the app never wrote to
// stdout) used to mask the previous attempt's tombstone snapshot.
// `rune logs gateway -n prod` would return exit 0 + empty body and
// operators saw nothing, even though a previous container had 14KB
// of real crash output. Fix: peek the live stream; if no data,
// surface the previous tombstone's LastLogs.
func TestGetServiceLogs_SilentLiveInstance_FallsBackToTombstone(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, controller := setupTestServiceController(t)
	service := createTestService(ctx, t, testStore, "test-service")

	// Live, Running, but the container has zero stdout/stderr.
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "live-silent", types.InstanceStatusRunning)
	// Previous attempt's tombstone WITH a captured snapshot.
	earlier := time.Now().Add(-2 * time.Minute)
	tomb := &types.Instance{
		ID: "tomb-with-logs", Name: "tomb-0", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status: types.InstanceStatusFailed, FailedAt: &earlier,
		LastLogs: []byte("previous-attempt-crash-trace"),
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", tomb.ID, tomb))

	// Stub: live instance returns empty stream, tombstone returns its LastLogs.
	fakeInstanceController.GetLogsFunc = func(ctx context.Context, instance *types.Instance, _ types.LogOptions) (io.ReadCloser, error) {
		if instance.ID == "live-silent" {
			return io.NopCloser(strings.NewReader("")), nil
		}
		return io.NopCloser(strings.NewReader(string(instance.LastLogs))), nil
	}

	rc, err := controller.GetServiceLogs(ctx, service.Namespace, service.Name, types.LogOptions{ShowLogs: true})
	require.NoError(t, err)
	body, _ := io.ReadAll(rc)
	rc.Close()
	assert.Contains(t, string(body), "previous-attempt-crash-trace",
		"silent live container must trigger tombstone fallback, not silent empty output")
}

// TestGetServiceLogs_LiveHasContent_NoFallback asserts the negative:
// when the live instance actually produces output, we don't append
// the tombstone snapshot — that would just be noise for healthy
// services.
func TestGetServiceLogs_LiveHasContent_NoFallback(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, controller := setupTestServiceController(t)
	service := createTestService(ctx, t, testStore, "test-service")
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "live-talking", types.InstanceStatusRunning)
	earlier := time.Now().Add(-1 * time.Minute)
	tomb := &types.Instance{
		ID: "tomb-with-logs", Name: "tomb-0", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status: types.InstanceStatusFailed, FailedAt: &earlier,
		LastLogs: []byte("OLD-LOGS-MUST-NOT-LEAK"),
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", tomb.ID, tomb))

	fakeInstanceController.GetLogsFunc = func(ctx context.Context, instance *types.Instance, _ types.LogOptions) (io.ReadCloser, error) {
		if instance.ID == "live-talking" {
			return io.NopCloser(strings.NewReader("LIVE-CONTENT-HERE")), nil
		}
		return io.NopCloser(strings.NewReader(string(instance.LastLogs))), nil
	}

	rc, err := controller.GetServiceLogs(ctx, service.Namespace, service.Name, types.LogOptions{ShowLogs: true})
	require.NoError(t, err)
	body, _ := io.ReadAll(rc)
	rc.Close()
	got := string(body)
	assert.Contains(t, got, "LIVE-CONTENT-HERE")
	assert.NotContains(t, got, "OLD-LOGS-MUST-NOT-LEAK",
		"tombstone fallback must NOT fire when live instance has content")
}

// TestIsStatusOnlyChange_GenerationBased verifies loop-suppression is driven by
// the persisted Generation vs ObservedGeneration pair (RFC #129 Phase 2). Every
// desired-state change bumps Generation — spec edits via cast AND scale changes,
// which the ScalingController now bumps Generation for — and the reconciler
// advances ObservedGeneration once it converges. Equal ⇒ status-only echo
// (skip); unequal ⇒ reconcile. This replaces the old scale-tracking shadow map:
// the restart scale-back-up now reconciles because writing the new Scale bumped
// Generation (asserted in the scaling controller test), not because a separate
// map noticed the scale differ.
func TestIsStatusOnlyChange_GenerationBased(t *testing.T) {
	_, _, _, _, controller := setupTestServiceController(t)
	sc := controller.(*serviceController)
	ns, name := "default", "web"

	// Generation 1, never reconciled (ObservedGeneration 0) → must reconcile.
	svc := &types.Service{Name: name, Namespace: ns, Scale: 1,
		Metadata: &types.ServiceMetadata{Generation: 1, ObservedGeneration: 0}}
	assert.False(t, sc.isStatusOnlyChange(svc), "unreconciled generation is not status-only")

	// Reconciler has converged on generation 1 → status-only echo, skip.
	svc.Metadata.ObservedGeneration = 1
	assert.True(t, sc.isStatusOnlyChange(svc), "converged generation is a status-only change")

	// A desired-state change (spec edit or scale) bumps Generation ahead of
	// ObservedGeneration → must reconcile. This is the restart scale-back-up path.
	svc.Metadata.Generation = 2
	assert.False(t, sc.isStatusOnlyChange(svc), "a generation ahead of observed must reconcile")

	// Once the reconciler converges on generation 2 → status-only again.
	svc.Metadata.ObservedGeneration = 2
	assert.True(t, sc.isStatusOnlyChange(svc), "re-converged generation is status-only")

	// Missing metadata → treated as needing reconciliation (defensive).
	assert.False(t, sc.isStatusOnlyChange(&types.Service{Name: name, Namespace: ns}),
		"service without metadata is not status-only")
}
