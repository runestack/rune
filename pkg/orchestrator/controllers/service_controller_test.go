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
