package controllers

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestController creates a controller with test dependencies
func setupTestController(t *testing.T) (context.Context, *store.TestStore, *runner.TestRunner, InstanceController) {
	ctx := context.Background()
	// Configure test store with reasonable defaults to support secret/config repos
	opts := store.StoreOptions{
		SecretEncryptionEnabled: true,
		KEKBytes:                []byte("0123456789abcdef0123456789abcdef"), // 32 bytes
		SecretLimits: store.Limits{
			MaxObjectBytes:   1 << 20, // 1MiB
			MaxKeyNameLength: 256,
		},
		ConfigLimits: store.Limits{
			MaxObjectBytes:   1 << 20,
			MaxKeyNameLength: 256,
		},
	}
	testStore := store.NewTestStoreWithOptions(opts)
	testRunner := runner.NewTestRunner()
	testRunnerMgr := manager.NewTestRunnerManager(nil)
	testRunnerMgr.SetDockerRunner(testRunner)
	testRunnerMgr.SetProcessRunner(testRunner)
	testLogger := log.NewLogger()

	controller := NewInstanceController(testStore, testRunnerMgr, testLogger)
	return ctx, testStore, testRunner, controller
}

// createTestService creates a test service in the store
func instanceControllerCreateTestService(ctx context.Context, t *testing.T, testStore *store.TestStore, name string, restartPolicy types.RestartPolicy) *types.Service {
	service := &types.Service{
		ID:            name,
		Name:          name,
		Namespace:     "default",
		RestartPolicy: restartPolicy,
		Image:         "test-image:latest",
		Command:       "test-command",
		Args:          []string{"arg1", "arg2"},
		Runtime:       "container",
		Env: map[string]string{
			"ENV_VAR1": "value1",
			"ENV_VAR2": "value2",
		},
		Metadata: &types.ServiceMetadata{
			Generation: 1,
		},
	}

	err := testStore.CreateService(ctx, service)
	require.NoError(t, err, "Failed to create test service")
	return service
}

// TestCreateInstance tests the CreateInstance method
func TestCreateInstance(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create a test service
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Test creating an instance for the service
	instance, err := controller.CreateInstance(ctx, service, "test-instance-0", 0)
	require.NoError(t, err, "CreateInstance should not return an error")
	assert.NotNil(t, instance, "Instance should not be nil")
	assert.Equal(t, "test-instance-0", instance.Name, "Instance Name should match")
	assert.Equal(t, service.ID, instance.ServiceID, "Instance should reference the service")
	assert.Equal(t, types.InstanceStatusRunning, instance.Status, "Instance should be running")

	// Verify instance was stored
	storedInstance, err := testStore.GetInstanceByID(ctx, "default", instance.ID)
	require.NoError(t, err, "Instance should be in the store")
	assert.Equal(t, instance.ID, storedInstance.ID, "Stored instance ID should match")

	// Verify runner calls were made
	assert.Contains(t, testRunner.CreatedInstances, instance, "Runner should have created the instance")
	assert.Contains(t, testRunner.StartedInstances, instance.ID, "Runner should have started the instance")

	// Verify environment variables
	assert.Contains(t, instance.Environment, "RUNE_SERVICE_NAME", "Should have service name env var")
	assert.Contains(t, instance.Environment, "ENV_VAR1", "Should have service env vars")
	assert.Equal(t, "value1", instance.Environment["ENV_VAR1"], "Env var should have correct value")
}

// TestDeleteInstance tests the DeleteInstance method
func TestDeleteInstance(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create a test service and instance
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Create instance directly in store and runner
	instance := &types.Instance{
		ID:        "test-instance",
		Name:      "test-instance",
		Namespace: "default",
		ServiceID: service.ID,
		Status:    types.InstanceStatusRunning,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	err := testStore.CreateInstance(ctx, instance)
	require.NoError(t, err, "Failed to create test instance")

	// Add to test runner's tracked instances
	err = testRunner.Create(ctx, instance)
	require.NoError(t, err, "Failed to add instance to runner")

	// Test deleting the instance
	err = controller.DeleteInstance(ctx, instance)
	require.NoError(t, err, "DeleteInstance should not return an error")

	// Verify instance status was updated
	storedInstance, err := testStore.GetInstanceByID(ctx, "default", "test-instance")
	require.NoError(t, err, "Instance should still be in store")
	assert.Equal(t, types.InstanceStatusDeleted, storedInstance.Status, "Instance status should be deleted")

	// Verify runner calls were made
	assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Runner should have stopped the instance")
	assert.Contains(t, testRunner.RemovedInstances, instance.ID, "Runner should have removed the instance")
}

// TestStopInstance tests the StopInstance method
func TestStopInstance(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create a test service and instance
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Create instance directly in store and runner
	instance := &types.Instance{
		ID:        "test-instance-stop",
		Name:      "test-instance-stop",
		Namespace: "default",
		ServiceID: service.ID,
		Status:    types.InstanceStatusRunning,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	err := testStore.CreateInstance(ctx, instance)
	require.NoError(t, err, "Failed to create test instance")

	// Add to test runner's tracked instances
	err = testRunner.Create(ctx, instance)
	require.NoError(t, err, "Failed to add instance to runner")

	// Test stopping the instance
	err = controller.StopInstance(ctx, instance)
	require.NoError(t, err, "StopInstance should not return an error")

	// Verify instance status was updated to stopped
	storedInstance, err := testStore.GetInstanceByID(ctx, "default", "test-instance-stop")
	require.NoError(t, err, "Instance should still be in store")
	assert.Equal(t, types.InstanceStatusStopped, storedInstance.Status, "Instance status should be stopped")
	assert.Equal(t, "Stopped by user", storedInstance.StatusMessage, "Status message should indicate stopped by user")

	// Verify runner stop call was made
	assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Runner should have stopped the instance")
}

// TestGetInstanceStatus tests the GetInstanceStatus method
func TestGetInstanceStatus(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create a test service and instance
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Create instance directly in store
	instance := &types.Instance{
		ID:        "test-instance",
		Name:      "test-instance",
		Namespace: "default",
		ServiceID: service.ID,
		Status:    types.InstanceStatusRunning,
		NodeID:    "test-node",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	err := testStore.CreateInstance(ctx, instance)
	require.NoError(t, err, "Failed to create test instance")

	// Set up test runner status
	testRunner.StatusResults[instance.ID] = types.InstanceStatusRunning

	// Test getting the instance status
	statusInfo, err := controller.GetInstanceStatus(ctx, instance)
	require.NoError(t, err, "GetInstanceStatus should not return an error")
	assert.Equal(t, types.InstanceStatusRunning, statusInfo.Status, "Status should be running")
	assert.Equal(t, instance.ID, statusInfo.InstanceID, "Instance ID should match")
	assert.Equal(t, instance.NodeID, statusInfo.NodeID, "Node ID should match")

	// Verify runner call was made
	assert.Contains(t, testRunner.StatusCalls, instance.ID, "Runner should have been called for status")
}

// TestGetInstanceLogs tests the GetInstanceLogs method
func TestGetInstanceLogs(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create a test service and instance
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Create instance directly in store
	instance := &types.Instance{
		ID:        "test-instance",
		Name:      "test-instance",
		Namespace: "default",
		ServiceID: service.ID,
		Status:    types.InstanceStatusRunning,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	err := testStore.CreateInstance(ctx, instance)
	require.NoError(t, err, "Failed to create test instance")

	// Set up test log output in runner
	testLogContent := []byte("Test log output\nLine 2\nLine 3")
	testRunner.LogOutput = testLogContent

	// Test getting the instance logs
	logOpts := types.LogOptions{
		Follow:     false,
		Tail:       10,
		Timestamps: false,
	}

	logs, err := controller.GetInstanceLogs(ctx, instance, logOpts)
	require.NoError(t, err, "GetInstanceLogs should not return an error")
	defer logs.Close()

	// Read log content
	content, err := io.ReadAll(logs)
	require.NoError(t, err, "Should be able to read logs")
	assert.Equal(t, testLogContent, content, "Log content should match")

	// Verify runner call was made
	assert.Contains(t, testRunner.LogCalls, instance.ID, "Runner should have been called for logs")
}

// TestExec tests the Exec method
func TestExec(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create a test service and instance
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Create instance directly in store
	instance := &types.Instance{
		ID:        "test-instance",
		Name:      "test-instance",
		Namespace: "default",
		ServiceID: service.ID,
		Status:    types.InstanceStatusRunning,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	err := testStore.CreateInstance(ctx, instance)
	require.NoError(t, err, "Failed to create test instance")

	// Test command to execute
	command := []string{"ls", "-la"}

	// Set up test output in runner
	testStdoutContent := []byte("total 123\ndrwxr-xr-x 7 user group 224 Feb 10 12:34 .\n")
	testStderrContent := []byte("Warning: some files not accessible\n")
	testExitCode := 0

	// Configure test runner's exec behavior
	testRunner.ExecOutput = testStdoutContent
	testRunner.ExecErrOutput = testStderrContent
	testRunner.ExitCodeVal = testExitCode

	// Create exec options
	execOpts := types.ExecOptions{
		Command:        command,
		Env:            map[string]string{"TEST_VAR": "test_value"},
		WorkingDir:     "/app",
		TTY:            true,
		TerminalWidth:  80,
		TerminalHeight: 24,
	}

	// Call Exec
	execStream, err := controller.Exec(ctx, instance, execOpts)
	require.NoError(t, err, "Exec should not return an error")
	require.NotNil(t, execStream, "ExecStream should not be nil")
	defer execStream.Close()

	// Read stdout
	stdoutBuf := make([]byte, 1024)
	n, err := execStream.Read(stdoutBuf)
	require.NoError(t, err, "Should be able to read stdout")
	assert.Equal(t, testStdoutContent, stdoutBuf[:n], "Stdout content should match")

	// Read stderr
	stderrReader := execStream.Stderr()
	stderrBuf := make([]byte, 1024)
	n, err = stderrReader.Read(stderrBuf)
	require.NoError(t, err, "Should be able to read stderr")
	assert.Equal(t, testStderrContent, stderrBuf[:n], "Stderr content should match")

	// Get exit code
	exitCode, err := execStream.ExitCode()
	require.NoError(t, err, "Should be able to get exit code")
	assert.Equal(t, testExitCode, exitCode, "Exit code should match")

	// Verify runner call was made
	assert.Contains(t, testRunner.ExecCalls, instance.ID, "Runner should have been called for exec")

	// Verify command was passed correctly - index is a number since we use a slice
	lastExecOpts := testRunner.ExecOptions[len(testRunner.ExecOptions)-1]
	assert.Equal(t, command, lastExecOpts.Command, "Command should match")
	assert.Equal(t, execOpts.Env, lastExecOpts.Env, "Environment variables should match")
	assert.Equal(t, execOpts.WorkingDir, lastExecOpts.WorkingDir, "Working directory should match")
	assert.Equal(t, execOpts.TTY, lastExecOpts.TTY, "TTY setting should match")
	assert.Equal(t, execOpts.TerminalWidth, lastExecOpts.TerminalWidth, "Terminal width should match")
	assert.Equal(t, execOpts.TerminalHeight, lastExecOpts.TerminalHeight, "Terminal height should match")
}

// TestRestartInstance tests the RestartInstance method
func TestRestartInstance(t *testing.T) {
	// Create test objects
	testStore := store.NewTestStore()
	testRunner := runner.NewTestRunner()
	testRunnerMgr := manager.NewTestRunnerManager(nil)
	testRunnerMgr.SetDockerRunner(testRunner)
	testRunnerMgr.SetProcessRunner(testRunner)
	testLogger := log.NewLogger()

	// Create the instance controller
	controller := NewInstanceController(testStore, testRunnerMgr, testLogger)

	// Create test instances
	instance := &types.Instance{
		ID:          "test-instance",
		Name:        "test-instance",
		Namespace:   "default",
		ServiceID:   "test-service",
		ServiceName: "test-service",
		Status:      types.InstanceStatusRunning,
	}

	ctx := context.Background()

	t.Run("SkipsTombstonedService", func(t *testing.T) {
		// RFC #129 Phase 4: a service being torn down must never have an
		// instance resurrected under it — the reconcileDeletion cascade owns
		// those instances, and a health-triggered replacement here would
		// orphan a container the retired store-orphan sweep used to catch.
		st := store.NewTestStore()
		rm := manager.NewTestRunnerManager(nil)
		tr := runner.NewTestRunner()
		rm.SetDockerRunner(tr)
		rm.SetProcessRunner(tr)
		c := NewInstanceController(st, rm, log.NewLogger())

		now := time.Now()
		deleting := &types.Service{
			ID: "svc-del", Name: "svc-del", Namespace: "default", Runtime: "container",
			RestartPolicy: types.RestartPolicyAlways,
			Metadata:      &types.ServiceMetadata{DeletionTimestamp: &now},
		}
		require.NoError(t, st.CreateService(ctx, deleting))
		inst := &types.Instance{
			ID: "svc-del-0", Name: "svc-del-0", Namespace: "default",
			ServiceID: "svc-del", ServiceName: "svc-del", Status: types.InstanceStatusRunning,
		}
		require.NoError(t, st.CreateInstance(ctx, inst))

		err := c.RestartInstance(ctx, inst, InstanceRestartReasonHealthCheckFailure)
		require.NoError(t, err, "restart on a deleting service must be a quiet no-op")
		assert.Empty(t, tr.CreatedInstances, "no replacement instance may be created for a tombstoned service")
	})

	t.Run("RestartPolicy=Always", func(t *testing.T) {
		// Create test service with Always restart policy
		serviceAlways := &types.Service{
			ID:            "test-service",
			Name:          "test-service",
			Namespace:     "default",
			RestartPolicy: types.RestartPolicyAlways,
			Runtime:       "container",
		}

		// Add service to store
		err := testStore.CreateService(ctx, serviceAlways)
		assert.NoError(t, err)

		// Add instance to store
		err = testStore.CreateInstance(ctx, instance)
		assert.NoError(t, err)

		// Call the method
		err = controller.RestartInstance(context.Background(), instance, InstanceRestartReasonManual)

		// Verify: new tombstone+replace semantics. The original instance
		// is stopped (preserved), then a NEW replacement instance with the
		// same logical Name (but fresh UUID) is Create+Start'd.
		assert.NoError(t, err)
		assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Original instance should have been stopped")
		require.NotEmpty(t, testRunner.CreatedInstances, "A replacement instance should have been created")
		assert.Equal(t, instance.Name, testRunner.CreatedInstances[len(testRunner.CreatedInstances)-1].Name,
			"Replacement should share the original's logical Name")
		assert.NotEqual(t, instance.ID, testRunner.CreatedInstances[len(testRunner.CreatedInstances)-1].ID,
			"Replacement should have a brand-new UUID")
		require.NotEmpty(t, testRunner.StartedInstances, "Replacement should have been started")
	})

	// Test with OnFailure restart policy
	t.Run("RestartPolicy=OnFailure", func(t *testing.T) {
		serviceOnFailure := &types.Service{
			ID:            "test-service",
			Name:          "test-service",
			Namespace:     "default",
			RestartPolicy: types.RestartPolicyOnFailure,
			Runtime:       "container",
		}

		// Reset between tests
		instance.Status = types.InstanceStatusRunning
		instance.FailedAt = nil
		instance.FailureReason = ""
		testStore.Reset()
		testRunner = runner.NewTestRunner() // Create a fresh runner
		testRunnerMgr = manager.NewTestRunnerManager(nil)
		testRunnerMgr.SetDockerRunner(testRunner)
		testRunnerMgr.SetProcessRunner(testRunner)
		controller = NewInstanceController(testStore, testRunnerMgr, testLogger)

		// Set up test data for OnFailure policy
		err := testStore.CreateService(ctx, serviceOnFailure)
		assert.NoError(t, err)

		err = testStore.CreateInstance(ctx, instance)
		assert.NoError(t, err)

		// Call with non-failure reason - should skip restart
		err = controller.RestartInstance(context.Background(), instance, InstanceRestartReasonUpdate)

		// Should not have any operations performed
		assert.NoError(t, err)
		assert.Empty(t, testRunner.StoppedInstances, "Instance should not have been stopped with update reason")
		assert.Empty(t, testRunner.CreatedInstances, "Instance should not have been created with update reason")
		assert.Empty(t, testRunner.StartedInstances, "Instance should not have been started with update reason")

		// Now call with failure reason - should restart.
		// New semantics: the original is tombstoned (Failed). Subsequent
		// RestartInstance calls against the same pointer no-op because
		// the store-loaded record reports Status=Failed (terminal state
		// short-circuit). The first failure call is the only one that
		// triggers Create/Start; re-build a fresh "live" instance to
		// re-exercise the path with different reasons.
		err = controller.RestartInstance(context.Background(), instance, InstanceRestartReasonFailure)
		assert.NoError(t, err)
		assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Original should have been stopped")
		require.NotEmpty(t, testRunner.CreatedInstances, "Replacement should have been created")
		assert.Equal(t, instance.Name, testRunner.CreatedInstances[len(testRunner.CreatedInstances)-1].Name)
	})

	// Test with Never restart policy
	t.Run("RestartPolicy=Never", func(t *testing.T) {
		serviceOnNever := &types.Service{
			ID:            "test-service",
			Name:          "test-service",
			Namespace:     "default",
			RestartPolicy: types.RestartPolicyNever,
			Runtime:       "container",
		}

		// Reset between tests. The shared `instance` pointer may carry
		// Failed status mutated by a prior subtest's RestartInstance; reset
		// it so this block starts from a clean Running state.
		instance.Status = types.InstanceStatusRunning
		instance.FailedAt = nil
		instance.FailureReason = ""
		testStore.Reset()
		testRunner = runner.NewTestRunner() // Create a fresh runner
		testRunnerMgr = manager.NewTestRunnerManager(nil)
		testRunnerMgr.SetDockerRunner(testRunner)
		testRunnerMgr.SetProcessRunner(testRunner)
		controller = NewInstanceController(testStore, testRunnerMgr, testLogger)

		err := testStore.CreateService(ctx, serviceOnNever)
		assert.NoError(t, err)

		err = testStore.CreateInstance(ctx, instance)
		assert.NoError(t, err)

		// Call with automatic restart reasons - should not restart
		err = controller.RestartInstance(context.Background(), instance, InstanceRestartReasonFailure)
		assert.NoError(t, err)
		assert.Empty(t, testRunner.StoppedInstances, "Instance should not have been stopped with failure reason")
		assert.Empty(t, testRunner.CreatedInstances, "Instance should not have been created with failure reason")
		assert.Empty(t, testRunner.StartedInstances, "Instance should not have been started with failure reason")

		err = controller.RestartInstance(context.Background(), instance, InstanceRestartReasonHealthCheckFailure)
		assert.NoError(t, err)
		assert.Empty(t, testRunner.StoppedInstances, "Instance should not have been stopped with health check failure reason")
		assert.Empty(t, testRunner.CreatedInstances, "Instance should not have been created with health check failure reason")
		assert.Empty(t, testRunner.StartedInstances, "Instance should not have been started with health check failure reason")

		err = controller.RestartInstance(context.Background(), instance, InstanceRestartReasonUpdate)
		assert.NoError(t, err)
		assert.Empty(t, testRunner.StoppedInstances, "Instance should not have been stopped with update reason")
		assert.Empty(t, testRunner.CreatedInstances, "Instance should not have been created with update reason")
		assert.Empty(t, testRunner.StartedInstances, "Instance should not have been started with update reason")

		// Reset runner for next test
		testRunner = runner.NewTestRunner()
		testRunnerMgr.SetDockerRunner(testRunner)
		testRunnerMgr.SetProcessRunner(testRunner)

		// Call with manual restart - should restart even with Never policy.
		// New semantics: original tombstoned, fresh replacement created.
		err = controller.RestartInstance(context.Background(), instance, InstanceRestartReasonManual)
		assert.NoError(t, err)
		assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Original should have been stopped (manual override)")
		require.NotEmpty(t, testRunner.CreatedInstances, "Replacement should have been created (manual override)")
		assert.Equal(t, instance.Name, testRunner.CreatedInstances[len(testRunner.CreatedInstances)-1].Name)
	})
}

// TestUpdateInstance tests the UpdateInstance method
func TestUpdateInstance(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create a test service
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Create instance directly in store
	originalUpdateTime := time.Now().Add(-1 * time.Hour) // Old update time
	instance := &types.Instance{
		ID:        "test-instance",
		Name:      "test-instance",
		Namespace: "default",
		ServiceID: service.ID,
		Status:    types.InstanceStatusRunning,
		Environment: map[string]string{
			"RUNE_SERVICE_NAME": "test-service",
			"ENV_VAR1":          "old-value", // This will be updated
		},
		Metadata: &types.InstanceMetadata{
			Image:             "original-image:latest",
			ServiceGeneration: 1, // Match the service generation
		},
		CreatedAt: time.Now(),
		UpdatedAt: originalUpdateTime,
	}

	err := testStore.CreateInstance(ctx, instance)
	require.NoError(t, err, "Failed to create test instance")

	// Set up test runner status
	testRunner.StatusResults[instance.ID] = types.InstanceStatusRunning

	// Modify the service to test updating the instance
	service.Env["ENV_VAR1"] = "new-value"   // Changed value
	service.Env["ENV_VAR3"] = "added-value" // New env var

	// Test updating the instance
	err = controller.UpdateInstance(ctx, service, instance)
	require.NoError(t, err, "UpdateInstance should not return an error")

	// Verify instance was updated in store
	updatedInstance, err := testStore.GetInstanceByID(ctx, "default", instance.ID)
	require.NoError(t, err, "Instance should be in the store")

	// Check that the environment was updated correctly
	assert.Equal(t, "new-value", updatedInstance.Environment["ENV_VAR1"], "ENV_VAR1 should be updated")
	assert.Equal(t, "added-value", updatedInstance.Environment["ENV_VAR3"], "ENV_VAR3 should be added")

	// Check that updateAt time is newer
	assert.NotEqual(t, originalUpdateTime, updatedInstance.UpdatedAt, "UpdatedAt should be changed")

	// Separate test for incompatible update
	TestUpdateInstanceIncompatible(t)
}

// TestUpdateInstanceIncompatible tests instance update with incompatible changes
func TestUpdateInstanceIncompatible(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)

	// Create service and instance that won't be compatible after changes
	service := &types.Service{
		ID:            "test-service-incompatible",
		Name:          "test-service-incompatible",
		Namespace:     "default",
		RestartPolicy: types.RestartPolicyAlways,
		Image:         "original-image:latest",
		Runtime:       "docker",
		Metadata: &types.ServiceMetadata{
			Generation: 1,
		},
	}

	err := testStore.CreateService(ctx, service)
	require.NoError(t, err, "Failed to create test service")

	// Create instance with original image
	instance := &types.Instance{
		ID:          "instance-to-recreate",
		Name:        "instance-to-recreate",
		Namespace:   "default",
		ServiceID:   service.ID,
		Status:      types.InstanceStatusRunning,
		ContainerID: "container123", // Important for docker runtime incompatibility check
		Metadata: &types.InstanceMetadata{
			Image: "original-image:latest",
		},
	}

	err = testStore.CreateInstance(ctx, instance)
	require.NoError(t, err, "Failed to create test instance")

	// Set up test runner status
	testRunner.StatusResults[instance.ID] = types.InstanceStatusRunning

	// Create a modified service with different image. An image change is a
	// TEMPLATE change, so cast stamps TemplateGeneration alongside Generation
	// (issue #142) — that's what triggers the incompatibility here.
	modifiedService := &types.Service{
		ID:            service.ID,
		Name:          service.Name,
		Namespace:     service.Namespace,
		RestartPolicy: service.RestartPolicy,
		Image:         "different-image:latest", // This should trigger incompatibility
		Runtime:       "docker",
		Metadata: &types.ServiceMetadata{
			Generation:         2,
			TemplateGeneration: 2,
		},
	}

	// Update should fail due to incompatibility
	err = controller.UpdateInstance(ctx, modifiedService, instance)
	assert.Error(t, err, "UpdateInstance should return an error for incompatible changes")
	assert.Contains(t, err.Error(), "requires recreation", "Error should indicate recreation is needed")
}

// TestIsInstanceCompatible_ScaleBumpDoesNotRecreate is the regression test for
// issue #142: the scaling controller bumps Generation on every scale write
// (RFC #129 Phase 2), and the old compatibility rule compared the instance's
// recorded generation against Generation — so every scale op container-bounced
// its surviving instances. The check now compares TemplateGeneration, which
// scale never touches.
func TestIsInstanceCompatible_ScaleBumpDoesNotRecreate(t *testing.T) {
	ctx, _, testRunner, controller := setupTestController(t)

	// Service after several scale ops: Generation raced ahead, template untouched.
	service := &types.Service{
		ID: "svc-scaled", Name: "svc-scaled", Namespace: "default",
		Image: "app:v1", Scale: 3,
		Metadata: &types.ServiceMetadata{
			Generation:         7, // bumped by scale ops
			TemplateGeneration: 1, // template unchanged since creation
		},
	}
	// Survivor created at template generation 1, still running.
	instance := &types.Instance{
		ID: "svc-scaled-abc", Name: "svc-scaled-abc", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status:   types.InstanceStatusRunning,
		Metadata: &types.InstanceMetadata{ServiceGeneration: 1},
	}
	testRunner.StatusResults[instance.ID] = types.InstanceStatusRunning

	compatible, reason := controller.isInstanceCompatibleWithService(ctx, instance, service)
	assert.True(t, compatible,
		"a scale-only Generation bump must NOT recreate surviving instances (issue #142); got reason: %s", reason)
}

// TestIsInstanceCompatible_TemplateChangeRecreates: a cast that changes the
// template stamps TemplateGeneration, and instances recorded at an older
// template must be recreated.
func TestIsInstanceCompatible_TemplateChangeRecreates(t *testing.T) {
	ctx, _, testRunner, controller := setupTestController(t)

	service := &types.Service{
		ID: "svc-recast", Name: "svc-recast", Namespace: "default",
		Image: "app:v2", Scale: 1,
		Metadata: &types.ServiceMetadata{
			Generation:         5,
			TemplateGeneration: 5, // cast just stamped it
		},
	}
	instance := &types.Instance{
		ID: "svc-recast-abc", Name: "svc-recast-abc", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status:   types.InstanceStatusRunning,
		Metadata: &types.InstanceMetadata{ServiceGeneration: 1}, // old template
	}
	testRunner.StatusResults[instance.ID] = types.InstanceStatusRunning

	compatible, reason := controller.isInstanceCompatibleWithService(ctx, instance, service)
	assert.False(t, compatible, "a template change must recreate old-template instances")
	assert.Contains(t, reason, "service template changed")
}

// TestIsInstanceCompatible_PreMigrationServiceDoesNotBounce: services that
// predate TemplateGeneration have 0 there, while their instances recorded
// old-semantics Generation values (> 0). Nothing may bounce on upgrade — only
// the next real cast (which stamps TemplateGeneration) recreates.
func TestIsInstanceCompatible_PreMigrationServiceDoesNotBounce(t *testing.T) {
	ctx, _, testRunner, controller := setupTestController(t)

	service := &types.Service{
		ID: "svc-legacy", Name: "svc-legacy", Namespace: "default",
		Image: "app:v1", Scale: 1,
		Metadata: &types.ServiceMetadata{
			Generation:         13, // months of history
			TemplateGeneration: 0,  // pre-migration record
		},
	}
	instance := &types.Instance{
		ID: "svc-legacy-abc", Name: "svc-legacy-abc", Namespace: "default",
		ServiceID: service.ID, ServiceName: service.Name,
		Status:   types.InstanceStatusRunning,
		Metadata: &types.InstanceMetadata{ServiceGeneration: 13}, // old semantics
	}
	testRunner.StatusResults[instance.ID] = types.InstanceStatusRunning

	compatible, reason := controller.isInstanceCompatibleWithService(ctx, instance, service)
	assert.True(t, compatible,
		"pre-migration services must not bounce instances on upgrade; got reason: %s", reason)
}

// TestInterpolateEnv_NonInterpolatedValue tests that regular environment variables are not modified
func TestInterpolateEnv_NonInterpolatedValue(t *testing.T) {
	ctx, _, _, controller := setupTestController(t)

	// Get the concrete instanceController to test interpolation
	instanceCtrl := controller.(*instanceController)

	// Test a regular environment variable value (no interpolation)
	val, err := instanceCtrl.interpolateEnv(ctx, "regular-value", "default")
	assert.NoError(t, err)
	assert.Equal(t, "regular-value", val)
}

// TestInterpolateEnv_TemplateSyntax tests template variable interpolation using table-driven tests
func TestInterpolateEnv_TemplateSyntax(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)

	// Get the concrete instanceController to test interpolation
	instanceCtrl := controller.(*instanceController)

	// Create test secrets and configmaps
	secret := &types.Secret{
		ID:        "test-secret",
		Name:      "test-secret",
		Namespace: "default",
		Data: map[string]string{
			"username": "admin",
			"password": "secret123",
		},
	}
	// Use SecretRepo to create, ensuring secrets are stored in encrypted StoredSecret form
	secretRepo := repos.NewSecretRepo(testStore)
	err := secretRepo.CreateRef(ctx, types.FormatRef(types.ResourceTypeSecret, "default", "test-secret"), secret)
	require.NoError(t, err)

	configmap := &types.Configmap{
		ID:        "test-config",
		Name:      "test-config",
		Namespace: "default",
		Data: map[string]string{
			"log-level": "debug",
			"app-name":  "test-app",
		},
	}
	err = testStore.Create(ctx, types.ResourceTypeConfigmap, "default", "test-config", configmap)
	require.NoError(t, err)

	tests := []struct {
		name        string
		input       string
		expected    string
		expectError bool
		errorMsg    string
	}{
		{
			name:     "Simple template variable",
			input:    "{{secret:test-secret/username}}",
			expected: "admin",
		},
		{
			name:     "Template variable with embedded text",
			input:    "{{configmap:test-config/app-name}}-prod",
			expected: "test-app-prod",
		},
		{
			name:     "Multiple template variables",
			input:    "{{secret:test-secret/username}}:{{secret:test-secret/password}}",
			expected: "admin:secret123",
		},
		{
			name:     "Template variable with default namespace",
			input:    "{{configmap:test-config/log-level}}",
			expected: "debug",
		},
		{
			name:     "Template variable with explicit namespace",
			input:    "{{configmap:test-config.default.rune/log-level}}",
			expected: "debug",
		},
		{
			name:     "Template variable with minimal shorthand",
			input:    "{{secret:test-secret/password}}",
			expected: "secret123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := instanceCtrl.interpolateEnv(ctx, tt.input, "default")
			if tt.expectError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, val)
			}
		})
	}
}

// TestInterpolateEnv_Errors tests error cases for template interpolation using table-driven tests
func TestInterpolateEnv_Errors(t *testing.T) {
	ctx, _, _, controller := setupTestController(t)

	// Get the concrete instanceController to test interpolation
	instanceCtrl := controller.(*instanceController)

	tests := []struct {
		name        string
		input       string
		expected    string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "Missing key in template variable",
			input:       "{{secret:test-secret}}",
			expected:    "",
			expectError: true,
			errorMsg:    "must include a key for interpolation",
		},
		{
			name:        "Invalid template variable format",
			input:       "{{invalid:format}}",
			expected:    "",
			expectError: true,
			errorMsg:    "must include a key for interpolation",
		},
		{
			name:        "Unsupported resource type",
			input:       "{{service:test-service/name}}",
			expected:    "",
			expectError: true,
			errorMsg:    "unsupported resource type",
		},
		{
			name:     "Malformed template syntax",
			input:    "{{unclosed",
			expected: "{{unclosed", // Should return as-is since it's not valid template syntax
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := instanceCtrl.interpolateEnv(ctx, tt.input, "default")
			if tt.expectError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
				assert.Equal(t, tt.expected, val)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, val)
			}
		})
	}
}

// TestPrepareEnvVars_WithoutInterpolation tests that environment variables without interpolation work correctly
func TestPrepareEnvVars_WithoutInterpolation(t *testing.T) {
	ctx, _, _, controller := setupTestController(t)

	// Get the concrete instanceController to test prepareEnvVars
	instanceCtrl := controller.(*instanceController)

	// Create a test service with regular environment variables (no interpolation needed)
	service := &types.Service{
		ID:        "test-service",
		Name:      "test-service",
		Namespace: "default",
		Env: map[string]string{
			"REGULAR_VAR": "regular-value",
			"ANOTHER_VAR": "another-value",
		},
		Ports: []types.ServicePort{
			{
				Name: "http",
				Port: 8080,
			},
		},
	}

	// Create a test instance
	instance := &types.Instance{
		ID:        "test-instance",
		Name:      "test-instance",
		Namespace: "default",
		ServiceID: "test-service",
	}

	// This should work since no interpolation is needed
	envVars, err := instanceCtrl.prepareEnvVars(ctx, service, instance)
	assert.NoError(t, err)
	assert.NotNil(t, envVars)

	// Check that regular env vars are preserved
	assert.Equal(t, "regular-value", envVars["REGULAR_VAR"])
	assert.Equal(t, "another-value", envVars["ANOTHER_VAR"])

	// Check built-in vars
	assert.Equal(t, "test-service", envVars["RUNE_SERVICE_NAME"])
	assert.Equal(t, "default", envVars["RUNE_SERVICE_NAMESPACE"])
	assert.Equal(t, "test-instance", envVars["RUNE_INSTANCE_ID"])

	// Check port vars
	assert.Equal(t, "8080", envVars["TEST_SERVICE_SERVICE_PORT"])
	assert.Equal(t, "8080", envVars["TEST_SERVICE_SERVICE_PORT_HTTP"])
}

// TestPrepareEnvVars_Basic tests the basic environment variable preparation functionality
func TestPrepareEnvVars_Basic(t *testing.T) {
	ctx, _, _, controller := setupTestController(t)

	// Get the concrete instanceController to test prepareEnvVars
	instanceCtrl := controller.(*instanceController)

	// Create a test service with some env vars and ports
	service := &types.Service{
		ID:        "test-service",
		Name:      "test-service",
		Namespace: "default",
		Env: map[string]string{
			"SERVICE_VAR1": "value1",
			"SERVICE_VAR2": "value2",
		},
		Ports: []types.ServicePort{
			{
				Name: "http",
				Port: 8080,
			},
			{
				Name: "metrics",
				Port: 9090,
			},
		},
	}

	// Create a test instance
	instance := &types.Instance{
		ID:        "test-instance-1",
		Name:      "test-instance-1",
		Namespace: "default",
		ServiceID: "test-service",
	}

	// Prepare environment variables
	envVars, err := instanceCtrl.prepareEnvVars(ctx, service, instance)
	assert.NoError(t, err, "prepareEnvVars should not return an error")
	assert.NotNil(t, envVars, "Environment variables should not be nil")

	// Check service-defined vars
	assert.Equal(t, "value1", envVars["SERVICE_VAR1"])
	assert.Equal(t, "value2", envVars["SERVICE_VAR2"])

	// Check built-in vars
	assert.Equal(t, "test-service", envVars["RUNE_SERVICE_NAME"])
	assert.Equal(t, "default", envVars["RUNE_SERVICE_NAMESPACE"])
	assert.Equal(t, "test-instance-1", envVars["RUNE_INSTANCE_ID"])

	// Check normalized vars
	assert.Equal(t, "test-service.default.rune", envVars["TEST_SERVICE_SERVICE_HOST"])
	assert.Equal(t, "8080", envVars["TEST_SERVICE_SERVICE_PORT"])
	assert.Equal(t, "8080", envVars["TEST_SERVICE_SERVICE_PORT_HTTP"])
	assert.Equal(t, "9090", envVars["TEST_SERVICE_SERVICE_PORT_METRICS"])
}

// TestPrepareEnvVars_HyphenatedNames tests environment variable preparation with hyphenated names
func TestPrepareEnvVars_HyphenatedNames(t *testing.T) {
	ctx, _, _, controller := setupTestController(t)

	// Get the concrete instanceController to test prepareEnvVars
	instanceCtrl := controller.(*instanceController)

	// Create a test service with hyphenated names
	service := &types.Service{
		ID:        "test-hyphenated-service",
		Name:      "test-hyphenated-service",
		Namespace: "test-ns",
		Ports: []types.ServicePort{
			{
				Name: "api-port",
				Port: 8000,
			},
		},
	}

	// Create a test instance
	instance := &types.Instance{
		ID:        "test-instance-2",
		Name:      "test-instance-2",
		Namespace: "test-ns",
		ServiceID: "test-hyphenated-service",
	}

	// Prepare environment variables
	envVars, err := instanceCtrl.prepareEnvVars(ctx, service, instance)
	assert.NoError(t, err, "prepareEnvVars should not return an error")
	assert.NotNil(t, envVars, "Environment variables should not be nil")

	// Check normalization of hyphenated names
	assert.Equal(t, "test-hyphenated-service.test-ns.rune", envVars["TEST_HYPHENATED_SERVICE_SERVICE_HOST"])
	assert.Equal(t, "8000", envVars["TEST_HYPHENATED_SERVICE_SERVICE_PORT"])
	assert.Equal(t, "8000", envVars["TEST_HYPHENATED_SERVICE_SERVICE_PORT_API_PORT"])
}

// TestPrepareEnvVars_WithTemplateInterpolation tests environment variable preparation with template interpolation
func TestPrepareEnvVars_WithTemplateInterpolation(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)

	// Get the concrete instanceController to test prepareEnvVars
	instanceCtrl := controller.(*instanceController)

	// Create test secrets and configmaps
	secret := &types.Secret{
		ID:        "db-credentials",
		Name:      "db-credentials",
		Namespace: "default",
		Data: map[string]string{
			"username": "dbuser",
			"password": "dbpass123",
		},
	}
	secretRepo := repos.NewSecretRepo(testStore)
	err := secretRepo.CreateRef(ctx, types.FormatRef(types.ResourceTypeSecret, "default", "db-credentials"), secret)
	require.NoError(t, err)

	configmap := &types.Configmap{
		ID:        "app-settings",
		Name:      "app-settings",
		Namespace: "default",
		Data: map[string]string{
			"log-level": "info",
			"app-name":  "my-app",
		},
	}
	err = testStore.Create(ctx, types.ResourceTypeConfigmap, "default", "app-settings", configmap)
	require.NoError(t, err)

	// Create a test service with template interpolation in environment variables
	service := &types.Service{
		ID:        "test-service-templates",
		Name:      "test-service-templates",
		Namespace: "default",
		Env: map[string]string{
			"DB_USERNAME": "{{secret:db-credentials/username}}",
			"DB_PASSWORD": "{{secret:db-credentials/password}}",
			"LOG_LEVEL":   "{{configmap:app-settings/log-level}}",
			"APP_NAME":    "{{configmap:app-settings/app-name}}-v1",
		},
	}

	// Create a test instance
	instance := &types.Instance{
		ID:        "test-instance-templates",
		Name:      "test-instance-templates",
		Namespace: "default",
		ServiceID: "test-service-templates",
	}

	// Prepare environment variables
	envVars, err := instanceCtrl.prepareEnvVars(ctx, service, instance)
	assert.NoError(t, err, "prepareEnvVars should not return an error")
	assert.NotNil(t, envVars, "Environment variables should not be nil")

	// Check that template variables were interpolated correctly
	assert.Equal(t, "dbuser", envVars["DB_USERNAME"])
	assert.Equal(t, "dbpass123", envVars["DB_PASSWORD"])
	assert.Equal(t, "info", envVars["LOG_LEVEL"])
	assert.Equal(t, "my-app-v1", envVars["APP_NAME"])

	// Check built-in vars are still present
	assert.Equal(t, "test-service-templates", envVars["RUNE_SERVICE_NAME"])
	assert.Equal(t, "default", envVars["RUNE_SERVICE_NAMESPACE"])
	assert.Equal(t, "test-instance-templates", envVars["RUNE_INSTANCE_ID"])
}

// TestPrepareEnvVars_EnvFrom covers import, prefix and precedence rules
func TestPrepareEnvVars_EnvFrom(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	instanceCtrl := controller.(*instanceController)

	// Prepare secret and configmap
	secret := &types.Secret{ID: "env-secrets", Name: "env-secrets", Namespace: "default", Data: map[string]string{
		"USER":     "admin",
		"PASSWORD": "s3cr3t",
	}}
	secretRepo := repos.NewSecretRepo(testStore)
	err := secretRepo.CreateRef(ctx, types.FormatRef(types.ResourceTypeSecret, "default", "env-secrets"), secret)
	require.NoError(t, err)

	cfg := &types.Configmap{ID: "app-settings", Name: "app-settings", Namespace: "default", Data: map[string]string{
		"LOG_LEVEL": "debug",
	}}
	err = testStore.Create(ctx, types.ResourceTypeConfigmap, "default", "app-settings", cfg)
	require.NoError(t, err)

	service := &types.Service{
		ID:        "svc",
		Name:      "svc",
		Namespace: "default",
		EnvFrom: []types.EnvFromSource{
			{SecretName: "env-secrets", Namespace: "default", Prefix: "APP_"},
			{ConfigmapName: "app-settings", Namespace: "default"},
		},
		// Explicit env overrides imported
		Env: map[string]string{
			"APP_USER": "override",
		},
	}

	instance := &types.Instance{ID: "i1", Name: "i1", Namespace: "default", ServiceID: "svc"}

	env, err := instanceCtrl.prepareEnvVars(ctx, service, instance)
	require.NoError(t, err)

	// Imported with prefix
	assert.Equal(t, "s3cr3t", env["APP_PASSWORD"])
	// Configmap imported without prefix
	assert.Equal(t, "debug", env["LOG_LEVEL"])
	// Explicit env overrides imported key
	assert.Equal(t, "override", env["APP_USER"])

	// Invalid key detection: create a bad secret and expect failure
	badSecret := &types.Secret{ID: "bad", Name: "bad", Namespace: "default", Data: map[string]string{
		"bad-key": "x",
	}}
	err = secretRepo.CreateRef(ctx, types.FormatRef(types.ResourceTypeSecret, "default", "bad"), badSecret)
	require.NoError(t, err)

	badService := &types.Service{ID: "svc2", Name: "svc2", Namespace: "default", EnvFrom: []types.EnvFromSource{
		{SecretName: "bad", Namespace: "default"},
	}}
	_, err = instanceCtrl.prepareEnvVars(ctx, badService, instance)
	require.Error(t, err)
}

// TestCreateInstance_SuccessSetsContainerEverCreatedAt asserts that a
// successful CreateInstance stamps ContainerEverCreatedAt and zeroes
// CreateAttempts. This is the load-bearing field used by the reconciler
// to tell "container vanished" apart from "create never succeeded";
// regressing this would re-introduce the churn loop documented in
// RUNE-BUG-RECONCILER-CHURN-ON-STABLE-PRECONDITION-FAILURE.
func TestCreateInstance_SuccessSetsContainerEverCreatedAt(t *testing.T) {
	ctx, _, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(t.Context(), t, controllerTestStore(controller), "test-service", types.RestartPolicyAlways)

	instance, err := controller.CreateInstance(ctx, service, "test-instance-0", 0)
	require.NoError(t, err)
	require.NotNil(t, instance.ContainerEverCreatedAt, "ContainerEverCreatedAt must be set after first successful Create")
	assert.Equal(t, 0, instance.CreateAttempts, "CreateAttempts should be reset to 0 on success")
}

// TestCreateInstance_RunnerCreateError_RecordsReason simulates a runner
// Create failure (the same shape as docker returning an error for an
// unresolvable image / volume / etc.) and asserts the instance record
// is updated in-place with the failure reason, NOT left at Pending
// with no detail. The user-facing payoff: `rune get instance -o yaml`
// now shows StatusMessage and FailureReason instead of an opaque
// Pending.
func TestCreateInstance_RunnerCreateError_RecordsReason(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Inject a runner-side create failure.
	testRunner.ErrorToReturn = assert.AnError
	_, err := controller.CreateInstance(ctx, service, "test-instance-0", 0)
	require.Error(t, err)

	// The record must still exist (no leaking-blank-Pending), with
	// StatusMessage + FailureReason populated and ContainerEverCreatedAt
	// left nil so the reconciler treats it as stuck-in-create.
	var stored []types.Instance
	require.NoError(t, testStore.List(ctx, types.ResourceTypeInstance, "default", &stored))
	require.Len(t, stored, 1, "Failed create must leave the record in place")
	rec := stored[0]
	assert.Equal(t, types.InstanceStatusFailed, rec.Status)
	assert.NotEmpty(t, rec.StatusMessage, "StatusMessage must surface the failure to operators")
	assert.NotEmpty(t, rec.FailureReason, "FailureReason must be set so it can be filtered/searched")
	assert.Equal(t, 1, rec.CreateAttempts, "CreateAttempts must increment on failure")
	assert.Nil(t, rec.ContainerEverCreatedAt, "ContainerEverCreatedAt must remain nil when Create never succeeded")
	assert.Nil(t, rec.FailedAt, "FailedAt must remain nil — stuck-in-create is not a tombstone (would skew retention GC)")
}

// TestIsInstanceCompatibleWithService_StuckInCreateHoldsSlot is the
// regression guard against the churn loop. A Failed record whose
// container never came up (ContainerEverCreatedAt == nil) must report
// as compatible so the reconciler does NOT tombstone+recreate-with-
// new-UUID every tick. The slot is held in place until an operator
// re-arms it via `rune restart instance`.
func TestIsInstanceCompatibleWithService_StuckInCreateHoldsSlot(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	stuck := &types.Instance{
		ID:                     "stuck-id",
		Name:                   "stuck-0",
		Namespace:              "default",
		ServiceID:              service.ID,
		ServiceName:            service.Name,
		Status:                 types.InstanceStatusFailed,
		StatusMessage:          "failed to resolve volume mount",
		FailureReason:          "VolumeNotReady",
		ContainerEverCreatedAt: nil, // never had a container
		Metadata:               &types.InstanceMetadata{ServiceGeneration: 1},
	}

	ok, reason := controller.isInstanceCompatibleWithService(ctx, stuck, service)
	assert.True(t, ok, "stuck-in-create record must claim its slot to break the churn loop")
	assert.Empty(t, reason)
}

// TestIsInstanceCompatibleWithService_VanishedContainerStillTriggersRecreate
// is the symmetrical regression guard: a record that WAS running
// (ContainerEverCreatedAt set) but whose container is gone from the
// runner (docker rm, host reboot, daemon crash) must still report
// incompatible so the existing tombstone+recreate path runs. Without
// this, real recovery scenarios silently stop working.
func TestIsInstanceCompatibleWithService_VanishedContainerStillTriggersRecreate(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	created := time.Now().Add(-1 * time.Hour)
	vanished := &types.Instance{
		ID:                     "vanished-id",
		Name:                   "vanished-0",
		Namespace:              "default",
		ServiceID:              service.ID,
		ServiceName:            service.Name,
		Runner:                 testRunner.Type(),
		Status:                 types.InstanceStatusRunning,
		ContainerEverCreatedAt: &created, // container did exist
		Metadata:               &types.InstanceMetadata{ServiceGeneration: 1},
	}
	// Container is gone: runner.Status will return error.
	testRunner.ErrorToReturn = assert.AnError

	ok, reason := controller.isInstanceCompatibleWithService(ctx, vanished, service)
	assert.False(t, ok, "vanished-container records must trigger recreate so the workload recovers")
	assert.Contains(t, reason, "not found in runner")
}

// controllerTestStore is a tiny adapter to get the underlying TestStore
// back from the interface-typed controller in tests that only carry the
// InstanceController and need the store too. It avoids changing setup
// helpers used by every other test in this file.
func controllerTestStore(c InstanceController) *store.TestStore {
	if cc, ok := c.(*instanceController); ok {
		if ts, ok := cc.store.(*store.TestStore); ok {
			return ts
		}
	}
	return nil
}

// TestCreateBackoffFor_ExponentialWithCap verifies the schedule
// 30s → 1m → 2m → 4m → 5m(cap) — the contract referenced by the
// reconciler retry-in-place branch and documented in PR2's description.
// Tightening this without coordinating with the reconciler tick (30s)
// risks either retry-storms or extending the time-to-Stalled past
// what operators expect.
func TestCreateBackoffFor_ExponentialWithCap(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 30 * time.Second},
		{2, 1 * time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 5 * time.Minute},  // capped
		{6, 5 * time.Minute},  // still capped
		{0, 30 * time.Second}, // clamps to attempt=1
	}
	for _, c := range cases {
		got := createBackoffFor(c.attempt)
		assert.Equal(t, c.want, got, "attempt %d", c.attempt)
	}
}

// TestRecordCreateFailure_SchedulesBackoff asserts that a non-terminal
// create failure populates NextCreateAttemptAt so the reconciler can
// honour the schedule. Without this, the reconciler would hit the
// retry path every tick and we'd be back to a 30s-cadence churn —
// just on the same UUID instead of new ones.
func TestRecordCreateFailure_SchedulesBackoff(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)
	testRunner.ErrorToReturn = assert.AnError

	before := time.Now()
	_, err := controller.CreateInstance(ctx, service, "test-instance-0", 0)
	require.Error(t, err)

	var stored []types.Instance
	require.NoError(t, testStore.List(ctx, types.ResourceTypeInstance, "default", &stored))
	require.Len(t, stored, 1)
	rec := stored[0]
	require.NotNil(t, rec.NextCreateAttemptAt, "NextCreateAttemptAt must be scheduled after a failed attempt")
	delay := rec.NextCreateAttemptAt.Sub(before)
	assert.InDelta(t, (30 * time.Second).Seconds(), delay.Seconds(), 2.0,
		"first-attempt backoff should be ~30s, got %s", delay)
	assert.Equal(t, types.InstanceStatusFailed, rec.Status,
		"first failures stay at Failed; Stalled only after retries exhaust")
}

// TestRecordCreateFailure_StallsAfterMaxAttempts walks the record
// past maxCreateAttempts and asserts it flips to Stalled with
// NextCreateAttemptAt cleared (operators see a clear "stop waiting"
// signal). This is the contract operators rely on to know when
// manual intervention is required vs. when to keep watching.
func TestRecordCreateFailure_StallsAfterMaxAttempts(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	rec := &types.Instance{
		ID:        "stuck-id",
		Name:      "stuck-0",
		Namespace: "default",
		ServiceID: "test-service",
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", rec.ID, rec))

	cc := controller.(*instanceController)
	// Walk attempts up to (max-1) — should still be Failed with backoff scheduled.
	for i := 0; i < maxCreateAttempts-1; i++ {
		cc.recordCreateFailure(ctx, rec, assert.AnError, "VolumeNotReady")
		assert.Equal(t, types.InstanceStatusFailed, rec.Status, "still Failed at attempt %d", rec.CreateAttempts)
		assert.NotNil(t, rec.NextCreateAttemptAt, "backoff scheduled at attempt %d", rec.CreateAttempts)
	}
	// The maxCreateAttempts-th failure flips to Stalled.
	cc.recordCreateFailure(ctx, rec, assert.AnError, "VolumeNotReady")
	assert.Equal(t, maxCreateAttempts, rec.CreateAttempts)
	assert.Equal(t, types.InstanceStatusStalled, rec.Status,
		"record must flip to Stalled after maxCreateAttempts failures")
	assert.Nil(t, rec.NextCreateAttemptAt,
		"Stalled records must NOT schedule auto-retry — operator must restart")
}

// TestIsInstanceCompatibleWithService_StalledHoldsSlot mirrors the
// existing stuck-in-create gate but for the terminal Stalled state.
// Without this, a Stalled record would be tombstoned by the
// reconciler and we'd lose the operator-visible "intervention
// required" signal.
func TestIsInstanceCompatibleWithService_StalledHoldsSlot(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	stalled := &types.Instance{
		ID:                     "stalled-id",
		Name:                   "stalled-0",
		Namespace:              "default",
		ServiceID:              service.ID,
		ServiceName:            service.Name,
		Status:                 types.InstanceStatusStalled,
		ContainerEverCreatedAt: nil,
		Metadata:               &types.InstanceMetadata{ServiceGeneration: 1},
	}
	ok, reason := controller.isInstanceCompatibleWithService(ctx, stalled, service)
	assert.True(t, ok, "Stalled stuck-in-create record must claim its slot")
	assert.Empty(t, reason)
}

// TestRetryCreateInstance_ResetsTransientStateAndPreservesAttempts
// asserts the retry contract: Status flips back to Pending and
// NextCreateAttemptAt is cleared (a new attempt is happening), but
// CreateAttempts is preserved (the backoff schedule needs to know how
// many failures have already happened so the NEXT failure schedules
// the correct next-backoff and the Stalled threshold is reached
// after the right number of cumulative failures).
func TestRetryCreateInstance_ResetsTransientStateAndPreservesAttempts(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Seed a stuck-in-create record (as if 2 attempts had already failed).
	earlier := time.Now().Add(-1 * time.Minute)
	rec := &types.Instance{
		ID:                  "stuck-id",
		Name:                "stuck-0",
		Namespace:           "default",
		ServiceID:           service.ID,
		ServiceName:         service.Name,
		Status:              types.InstanceStatusFailed,
		StatusMessage:       "old error",
		FailureReason:       "VolumeNotReady",
		CreateAttempts:      2,
		NextCreateAttemptAt: &earlier,
		Metadata:            &types.InstanceMetadata{ServiceGeneration: 1},
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", rec.ID, rec))

	// Inject another failure so we can verify the post-retry state without success.
	testRunner.ErrorToReturn = assert.AnError
	err := controller.RetryCreateInstance(ctx, service, rec)
	require.Error(t, err, "retry must surface the new failure to the caller")

	var stored []types.Instance
	require.NoError(t, testStore.List(ctx, types.ResourceTypeInstance, "default", &stored))
	require.Len(t, stored, 1, "retry must not create a new UUID — same slot, same record")
	got := stored[0]
	assert.Equal(t, "stuck-id", got.ID, "UUID preserved")
	assert.Equal(t, 3, got.CreateAttempts, "CreateAttempts must increment cumulatively across retries")
	assert.NotNil(t, got.NextCreateAttemptAt, "next backoff scheduled")
	assert.NotEqual(t, "old error", got.StatusMessage, "StatusMessage refreshed to the new failure")
}

// TestRestartInstance_StuckInCreateReusesSameRecord asserts the
// operator-action path: `rune restart instance` on a Stalled (or
// Failed-with-backoff) stuck-in-create record clears CreateAttempts
// and re-runs the create pipeline against the SAME UUID, instead of
// the tombstone+replace dance used for Running instances. Operators
// following an instance ID don't have to chase a moving identifier
// just because they restarted a stuck slot.
func TestRestartInstance_StuckInCreateReusesSameRecord(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	// Seed a Stalled stuck-in-create record (the case operators most
	// commonly encounter — retries exhausted, waiting for them to act).
	rec := &types.Instance{
		ID:             "stalled-id",
		Name:           "stalled-0",
		Namespace:      "default",
		ServiceID:      service.ID,
		ServiceName:    service.Name,
		Status:         types.InstanceStatusStalled,
		StatusMessage:  "old stalled reason",
		FailureReason:  "VolumeNotReady",
		CreateAttempts: maxCreateAttempts,
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", rec.ID, rec))

	require.NoError(t, controller.RestartInstance(ctx, rec, InstanceRestartReasonManual))

	var stored []types.Instance
	require.NoError(t, testStore.List(ctx, types.ResourceTypeInstance, "default", &stored))
	require.Len(t, stored, 1, "manual restart on stuck-in-create must NOT spawn a new UUID")
	got := stored[0]
	assert.Equal(t, "stalled-id", got.ID, "same UUID preserved across operator restart")
	// CreateAttempts was reset to 0 before the retry, then the (successful)
	// retry zeroes it again at the Running transition. Either way, must be 0.
	assert.Equal(t, 0, got.CreateAttempts, "operator restart resets attempt counter")
	assert.Equal(t, types.InstanceStatusRunning, got.Status, "successful retry promotes to Running")
}

// TestGetInstanceLogs_FallsBackToLastLogsWhenRunnerHasNoContainer is the
// regression guard for bug RUNE-BUG-RUNE-LOGS-IGNORES-TOMBSTONE-LASTLOGS:
// when the runner can't serve container logs (container removed by
// retention GC, or the tombstone-recreate path took it down), the
// LastLogs snapshot must be surfaced instead. Without this,
// `rune logs <failed-id>` and `rune logs <service>` go dark right when
// operators need them most.
func TestGetInstanceLogs_FallsBackToLastLogsWhenRunnerHasNoContainer(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	_ = instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	tomb := &types.Instance{
		ID:        "tomb-id",
		Name:      "tomb-0",
		Namespace: "default",
		Runner:    testRunner.Type(),
		Status:    types.InstanceStatusFailed,
		LastLogs:  []byte("captured stderr from the failing container\n"),
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", tomb.ID, tomb))
	// Container is gone from the runner.
	testRunner.ErrorToReturn = assert.AnError

	rc, err := controller.GetInstanceLogs(ctx, tomb, types.LogOptions{})
	require.NoError(t, err)
	defer rc.Close()
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "captured stderr from the failing container\n", string(body),
		"LastLogs snapshot must be served when the runner has no container")
}

// TestGetInstanceLogs_TerminalNoLogs_SynthesizesWhyLine asserts the
// crashed-container-with-no-stdout UX: when an instance is in a
// terminal state and has no LastLogs (common case: container PID 1
// SIGKILL'd by a failed health check before printing anything),
// `rune logs` returns a synthesized one-liner explaining the
// failure rather than silent empty output. Without this, operators
// see exit 0 / empty body from `rune logs <crashed-id>` and assume
// the CLI is broken.
func TestGetInstanceLogs_TerminalNoLogs_SynthesizesWhyLine(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	_ = instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	at := time.Now()
	tomb := &types.Instance{
		ID:            "empty-tomb",
		Name:          "empty-0",
		Namespace:     "default",
		Runner:        testRunner.Type(),
		Status:        types.InstanceStatusFailed,
		FailureReason: "HealthCheckFailure",
		StatusMessage: "Preserved for postmortem after health-check-failure",
		FailedAt:      &at,
		// LastLogs intentionally empty — container produced no output.
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", tomb.ID, tomb))
	testRunner.ErrorToReturn = assert.AnError

	rc, err := controller.GetInstanceLogs(ctx, tomb, types.LogOptions{})
	require.NoError(t, err, "terminal instances must yield SOMETHING from rune logs, not an error")
	defer rc.Close()
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	got := string(body)
	assert.Contains(t, got, "empty-tomb", "synthesized line must identify the instance")
	assert.Contains(t, got, "Failed", "synthesized line must include the terminal status")
	assert.Contains(t, got, "HealthCheckFailure", "synthesized line must include FailureReason so operators know why")
	assert.Contains(t, got, "no captured output", "synthesized line must say no logs were captured (vs. truncated)")
}

// TestDeleteInstance_SnapshotsLastLogsBeforeTearDown is the
// regression guard that closes the loop on bug
// RUNE-BUG-RUNE-LOGS-IGNORES-TOMBSTONE-LASTLOGS: the LastLogs field
// existed in the type but was never populated, so the
// service-level tombstone fallback (GetServiceLogs → most-recent
// tombstone with LastLogs) had nothing to read. DeleteInstance is
// the lifecycle moment that destroys the container; if we don't
// snapshot here, the only postmortem trail (LastLogs) goes with it.
func TestDeleteInstance_SnapshotsLastLogsBeforeTearDown(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	_ = instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	created := time.Now().Add(-1 * time.Hour)
	rec := &types.Instance{
		ID:                     "with-container",
		Name:                   "doomed-0",
		Namespace:              "default",
		Runner:                 testRunner.Type(),
		Status:                 types.InstanceStatusRunning,
		ContainerEverCreatedAt: &created, // had a container
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", rec.ID, rec))
	testRunner.LogOutput = []byte("captured stderr from the dying container\n")

	require.NoError(t, controller.DeleteInstance(ctx, rec))

	var stored types.Instance
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", rec.ID, &stored))
	assert.Equal(t, types.InstanceStatusDeleted, stored.Status)
	assert.NotEmpty(t, stored.LastLogs, "DeleteInstance must snapshot LastLogs before tearing the container down")
	assert.Equal(t, "captured stderr from the dying container\n", string(stored.LastLogs))
	require.NotNil(t, stored.LastLogsCapturedAt)
}

// TestSnapshotInstanceLogs_NoOpForNeverCreated guards the
// optimisation that skips snapshotting when there was no container
// in the first place (precondition-failed records). Without this
// the snapshot would invoke the runner with no container to look
// at, generating noise.
func TestSnapshotInstanceLogs_NoOpForNeverCreated(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	_ = instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	rec := &types.Instance{
		ID:                     "never-had-container",
		Name:                   "stuck-0",
		Namespace:              "default",
		Runner:                 testRunner.Type(),
		Status:                 types.InstanceStatusFailed,
		ContainerEverCreatedAt: nil,
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", rec.ID, rec))
	testRunner.LogOutput = []byte("should not be read")

	cc := controller.(*instanceController)
	cc.snapshotInstanceLogs(ctx, rec)

	assert.Empty(t, rec.LastLogs, "stuck-in-create records (no container) must not trigger a runner.GetLogs call")
}

// TestGetInstanceLogs_LiveSilentContainer_FallsBackToLastLogs is the
// per-instance counterpart of
// TestGetServiceLogs_SilentLiveInstance_FallsBackToTombstone:
// `rune logs <instance-id>` on a container that's running but
// producing zero stdout/stderr must still surface the LastLogs
// snapshot from the previous attempt. The bug originally manifested
// at the service level on prod/gateway but the per-instance code
// path had the same gap — a single zero-byte successful read from
// the runner masked everything else.
func TestGetInstanceLogs_LiveSilentContainer_FallsBackToLastLogs(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	_ = instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	rec := &types.Instance{
		ID:        "silent-live",
		Name:      "silent-0",
		Namespace: "default",
		Runner:    testRunner.Type(),
		Status:    types.InstanceStatusRunning,
		LastLogs:  []byte("previous-attempt-output\n"),
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", rec.ID, rec))
	// Runner returns empty bytes successfully — the docker-quiet case.
	testRunner.LogOutput = []byte("")

	rc, err := controller.GetInstanceLogs(ctx, rec, types.LogOptions{})
	require.NoError(t, err)
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	assert.Equal(t, "previous-attempt-output\n", string(body),
		"silent live container must fall back to LastLogs, not return empty body")
}

// TestCreateInstance_WithReadinessProbe_StaysStartingUntilProbePasses
// asserts the readiness gate added in this PR: when a service
// defines a readiness probe, the freshly-created instance must NOT
// flip to Running on runner.Start success — it stays Starting until
// the health controller observes the first readiness pass. Without
// this, prod/gateway showed Status=Running for ~30s before the
// liveness probe killed it, even though the app was never ready to
// serve traffic.
func TestCreateInstance_WithReadinessProbe_StaysStartingUntilProbePasses(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)
	service.Health = &types.HealthCheck{
		Readiness: &types.Probe{Type: "http", Path: "/ready", Port: 8080},
	}
	// Persist the updated spec so prepareEnvVars / etc. see the same one.
	require.NoError(t, testStore.Update(ctx, types.ResourceTypeService, service.Namespace, service.Name, service))

	instance, err := controller.CreateInstance(ctx, service, "ready-gated-0", 0)
	require.NoError(t, err)
	assert.Equal(t, types.InstanceStatusStarting, instance.Status,
		"with a readiness probe defined, runner.Start must NOT promote to Running")
	assert.Contains(t, instance.StatusMessage, "readiness probe",
		"status message must signal what we're waiting on")
}

// TestCreateInstance_NoReadinessProbe_PromotesToRunningAsBefore
// is the regression guard for the unchanged path: services WITHOUT
// a readiness probe still flip to Running on runner.Start (no
// probe = no signal = trust the runner). Tightening too far here
// would surprise every service that omitted readiness (i.e. most
// of them today).
func TestCreateInstance_NoReadinessProbe_PromotesToRunningAsBefore(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)
	service := instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)
	// No service.Health → no readiness probe.

	instance, err := controller.CreateInstance(ctx, service, "no-probe-0", 0)
	require.NoError(t, err)
	assert.Equal(t, types.InstanceStatusRunning, instance.Status)
}

// TestDeleteInstance_FlipsToTerminatingBeforeRunnerStop is the
// regression guard for the operator-visible UX gap reported live:
// after `rune delete service`, the instance kept showing Running
// for ~10s while runner.Stop's graceful-shutdown window elapsed.
// DeleteInstance must persist Status=Terminating BEFORE entering
// runner.Stop so `rune get instances` shows the truth immediately.
func TestDeleteInstance_FlipsToTerminatingBeforeRunnerStop(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	_ = instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	live := &types.Instance{
		ID:        "live-id",
		Name:      "live-0",
		Namespace: "default",
		Runner:    testRunner.Type(),
		Status:    types.InstanceStatusRunning,
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", live.ID, live))

	// Intercept the runner.Stop call to inspect the store mid-teardown.
	// The store update to Terminating must have happened by the time
	// the runner sees the Stop call.
	var sawTerminating bool
	testRunner.StopFunc = func(ctx context.Context, instance *types.Instance, _ time.Duration) error {
		var current types.Instance
		require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", "live-id", &current))
		if current.Status == types.InstanceStatusTerminating {
			sawTerminating = true
		}
		return nil
	}

	require.NoError(t, controller.DeleteInstance(ctx, live))
	assert.True(t, sawTerminating,
		"Status=Terminating must be persisted BEFORE runner.Stop is invoked, not after")

	// And after the teardown completes, the final state is Deleted.
	var final types.Instance
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", "live-id", &final))
	assert.Equal(t, types.InstanceStatusDeleted, final.Status)
}

// TestDeleteInstance_DoesNotResurrectTerminalStateAsTerminating
// guards against the symmetrical hazard: if an instance is already
// Failed (a postmortem tombstone) or Stalled (retries exhausted),
// the in-place Terminating flip must NOT overwrite that. Otherwise
// `rune logs <tomb>` would lose the "Failed" signal mid-cleanup.
func TestDeleteInstance_DoesNotResurrectTerminalStateAsTerminating(t *testing.T) {
	ctx, testStore, testRunner, controller := setupTestController(t)
	_ = instanceControllerCreateTestService(ctx, t, testStore, "test-service", types.RestartPolicyAlways)

	at := time.Now()
	tomb := &types.Instance{
		ID:        "failed-id",
		Name:      "tomb-0",
		Namespace: "default",
		Runner:    testRunner.Type(),
		Status:    types.InstanceStatusFailed,
		FailedAt:  &at,
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "default", tomb.ID, tomb))

	var seenStatusAtStop types.InstanceStatus
	testRunner.StopFunc = func(ctx context.Context, instance *types.Instance, _ time.Duration) error {
		var current types.Instance
		_ = testStore.Get(ctx, types.ResourceTypeInstance, "default", "failed-id", &current)
		seenStatusAtStop = current.Status
		return nil
	}

	require.NoError(t, controller.DeleteInstance(ctx, tomb))
	assert.Equal(t, types.InstanceStatusFailed, seenStatusAtStop,
		"Failed tombstones must NOT be transitioned to Terminating during DeleteInstance — they preserve postmortem state")
}
