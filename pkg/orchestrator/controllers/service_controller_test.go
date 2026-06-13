package controllers

import (
	"context"
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
func setupTestServiceController(t *testing.T) (context.Context, *store.TestStore, *FakeInstanceController, *FakeHealthController, *FakeScalingController, ServiceController) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	testInstanceController := NewFakeInstanceController()
	mockHealthController := NewFakeHealthController()
	mockScalingController := NewFakeScalingController()
	testLogger := log.NewLogger()

	controller, err := NewServiceController(
		testStore,
		testInstanceController,
		mockHealthController,
		mockScalingController,
		testLogger,
	)
	require.NoError(t, err, "Failed to create service controller")

	return ctx, testStore, testInstanceController, mockHealthController, mockScalingController, controller
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
	ctx, _, _, _, _, controller := setupTestServiceController(t)

	// Start the controller
	err := controller.Start(ctx)
	require.NoError(t, err, "Starting service controller should not error")

	// Stop the controller
	err = controller.Stop()
	require.NoError(t, err, "Stopping service controller should not error")
}

// TestGetServiceStatus tests the GetServiceStatus method
func TestGetServiceStatus(t *testing.T) {
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

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
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

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
	ctx, testStore, fakeInstanceController, _, _, controller := setupTestServiceController(t)

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
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

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
	ctx, testStore, fakeInstanceController, _, _, controller := setupTestServiceController(t)

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
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

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

// TestRestartService tests the RestartService method
func TestRestartService(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Create test instances
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-1", types.InstanceStatusRunning)
	serviceControllerCreateTestInstance(ctx, t, testStore, service.Name, "instance-2", types.InstanceStatusRunning)

	// Restart service
	err := controller.RestartService(ctx, service.Namespace, service.Name)
	require.NoError(t, err, "RestartService should not return an error")

	// Verify restart was called on all instances
	assert.Len(t, fakeInstanceController.RestartInstanceCalls, 2, "Restart should be called on both instances")
}

// TestStopService tests the StopService method
func TestStopService(t *testing.T) {
	ctx, testStore, fakeInstanceController, _, _, controller := setupTestServiceController(t)

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

// TestListInstancesForService tests the ListInstancesForService method
func TestListInstancesForService(t *testing.T) {
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

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
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

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
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

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
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

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
func TestHandleServiceCreated(t *testing.T) {
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

	// Create a test service
	service := &types.Service{
		ID:        "test-service",
		Name:      "test-service",
		Namespace: "default",
		Image:     "test-image:latest",
		Scale:     2,
		Status:    types.ServiceStatusPending,
		Metadata: &types.ServiceMetadata{
			Generation: 1,
		},
	}

	// Create the service in the store first
	err := testStore.Create(ctx, types.ResourceTypeService, service.Namespace, service.Name, service)
	require.NoError(t, err, "Failed to create service in store")

	// Handle service created
	err = controller.handleServiceCreated(ctx, service)
	require.NoError(t, err, "HandleServiceCreated should not return an error")

	// Verify service was updated in store
	var storedService types.Service
	err = testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &storedService)
	require.NoError(t, err, "Service should be stored")
	assert.Equal(t, service.Name, storedService.Name, "Service name should match")
}

// TestHandleServiceUpdated tests the HandleServiceUpdated method
func TestHandleServiceUpdated(t *testing.T) {
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Update service
	service.Scale = 3
	service.Metadata.Generation = 2

	// Handle service updated
	err := controller.handleServiceUpdated(ctx, service)
	require.NoError(t, err, "HandleServiceUpdated should not return an error")

	// Verify service was not updated in store (HandleServiceUpdated only triggers reconciliation)
	var storedService types.Service
	err = testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &storedService)
	require.NoError(t, err, "Service should be stored")
	assert.Equal(t, 2, storedService.Scale, "Service scale should remain unchanged in store")
}

// TestHandleServiceDeleted tests the HandleServiceDeleted method
func TestHandleServiceDeleted(t *testing.T) {
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Handle service deleted
	err := controller.handleServiceDeleted(ctx, service)
	require.NoError(t, err, "HandleServiceDeleted should not return an error")

	// Service deletion is handled by finalizers, so no immediate action is taken
	// The service should still exist in the store
	var storedService types.Service
	err = testStore.Get(ctx, types.ResourceTypeService, service.Namespace, service.Name, &storedService)
	require.NoError(t, err, "Service should still exist in store")
}

// TestProcessServiceEvent tests the ProcessServiceEvent method
func TestProcessServiceEvent(t *testing.T) {
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Test created event
	event := store.WatchEvent{
		Type:         store.WatchEventCreated,
		ResourceType: types.ResourceTypeService,
		Namespace:    service.Namespace,
		Name:         service.Name,
	}

	err := controller.processServiceEvent(ctx, event)
	require.NoError(t, err, "processServiceEvent should not return an error for created event")

	// Test updated event
	event.Type = store.WatchEventUpdated
	err = controller.processServiceEvent(ctx, event)
	require.NoError(t, err, "processServiceEvent should not return an error for updated event")

	// Test deleted event
	event.Type = store.WatchEventDeleted
	err = controller.processServiceEvent(ctx, event)
	require.NoError(t, err, "processServiceEvent should not return an error for deleted event")
}

// TestProcessServiceEventUnknownType tests ProcessServiceEvent with unknown event type
func TestProcessServiceEventUnknownType(t *testing.T) {
	ctx, testStore, _, _, _, controller := setupTestServiceController(t)

	// Create a test service
	service := createTestService(ctx, t, testStore, "test-service")

	// Test unknown event type
	event := store.WatchEvent{
		Type:         "unknown",
		ResourceType: types.ResourceTypeService,
		Namespace:    service.Namespace,
		Name:         service.Name,
	}

	err := controller.processServiceEvent(ctx, event)
	require.Error(t, err, "processServiceEvent should return an error for unknown event type")
	assert.Contains(t, err.Error(), "unknown event type", "Error should mention unknown event type")
}

// TestProcessServiceEventNotFound tests ProcessServiceEvent when service is not found
func TestProcessServiceEventNotFound(t *testing.T) {
	ctx, _, _, _, _, controller := setupTestServiceController(t)

	// Test event for non-existent service
	event := store.WatchEvent{
		Type:         store.WatchEventUpdated,
		ResourceType: types.ResourceTypeService,
		Namespace:    "default",
		Name:         "non-existent-service",
	}

	err := controller.processServiceEvent(ctx, event)
	require.Error(t, err, "processServiceEvent should return an error when service is not found")
	assert.Contains(t, err.Error(), "failed to get service", "Error should mention failed to get service")
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
	ctx, testStore, fakeInstanceController, _, _, controller := setupTestServiceController(t)
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
	ctx, testStore, fakeInstanceController, _, _, controller := setupTestServiceController(t)
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
	ctx, testStore, fakeInstanceController, _, _, controller := setupTestServiceController(t)
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
	ctx, testStore, fakeInstanceController, _, _, controller := setupTestServiceController(t)
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
	ctx, testStore, fakeInstanceController, _, _, controller := setupTestServiceController(t)
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

// TestIsStatusOnlyChange_ScaleChangeReconciles guards the fix for the restart
// "hang": scale changes are applied by the ScalingController WITHOUT bumping
// Generation, so a scale-only update used to look like a status-only change and
// get skipped — leaving the new replicas (and `rune restart`'s scale-back-up)
// uncreated until the next 30s reconcile tick. isStatusOnlyChange must treat a
// scale change as reconcile-worthy.
func TestIsStatusOnlyChange_ScaleChangeReconciles(t *testing.T) {
	_, _, _, _, _, controller := setupTestServiceController(t)
	sc := controller.(*serviceController)
	ns, name := "default", "web"

	svc := &types.Service{Name: name, Namespace: ns, Scale: 1, Metadata: &types.ServiceMetadata{Generation: 1}}

	// Never observed before → must reconcile.
	assert.False(t, sc.isStatusOnlyChange(svc), "unseen service is not a status-only change")

	// Simulate having reconciled at generation 1, scale 1.
	sc.recordObservedGeneration(ns, name, 1)
	sc.recordObservedScale(ns, name, 1)

	// Same generation, same scale → genuine status-only update, skip.
	assert.True(t, sc.isStatusOnlyChange(svc), "same gen + same scale is status-only")

	// Scale changed (1 → 3) with the SAME generation → must reconcile.
	svc.Scale = 3
	assert.False(t, sc.isStatusOnlyChange(svc), "a scale change must trigger reconciliation even at the same generation")

	// Scale back to a value we never reconciled at → must reconcile.
	svc.Scale = 0
	assert.False(t, sc.isStatusOnlyChange(svc), "scale-to-0 (restart drain) must trigger reconciliation")

	// Generation bump still forces reconcile regardless of scale.
	svc.Scale = 1
	svc.Metadata.Generation = 2
	assert.False(t, sc.isStatusOnlyChange(svc), "a generation bump is never status-only")
}
