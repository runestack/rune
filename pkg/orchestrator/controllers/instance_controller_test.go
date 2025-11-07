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
	instance, err := controller.CreateInstance(ctx, service, "test-instance-0")
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

		// Verify results
		assert.NoError(t, err)
		assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Instance should have been stopped")
		assert.Contains(t, testRunner.CreatedInstances, instance, "Instance should have been created")
		assert.Contains(t, testRunner.StartedInstances, instance.ID, "Instance should have been started")
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

		// Now call with failure reason - should restart
		err = controller.RestartInstance(context.Background(), instance, InstanceRestartReasonFailure)

		// Verify that operations were performed
		assert.NoError(t, err)
		assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Instance should have been stopped with failure reason")
		assert.Contains(t, testRunner.CreatedInstances, instance, "Instance should have been created with failure reason")
		assert.Contains(t, testRunner.StartedInstances, instance.ID, "Instance should have been started with failure reason")

		// Reset runner for next test
		testRunner = runner.NewTestRunner()
		testRunnerMgr.SetDockerRunner(testRunner)
		testRunnerMgr.SetProcessRunner(testRunner)

		// Call with health check failure reason - should restart
		err = controller.RestartInstance(context.Background(), instance, InstanceRestartReasonHealthCheckFailure)

		// Verify that operations were performed for health check failure
		assert.NoError(t, err)
		assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Instance should have been stopped with health check failure reason")
		assert.Contains(t, testRunner.CreatedInstances, instance, "Instance should have been created with health check failure reason")
		assert.Contains(t, testRunner.StartedInstances, instance.ID, "Instance should have been started with health check failure reason")

		// Reset runner for next test
		testRunner = runner.NewTestRunner()
		testRunnerMgr.SetDockerRunner(testRunner)
		testRunnerMgr.SetProcessRunner(testRunner)

		// Now call with manual restart - should always restart regardless of policy
		err = controller.RestartInstance(context.Background(), instance, InstanceRestartReasonManual)
		assert.NoError(t, err)
		assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Instance should have been stopped with manual reason")
		assert.Contains(t, testRunner.CreatedInstances, instance, "Instance should have been created with manual reason")
		assert.Contains(t, testRunner.StartedInstances, instance.ID, "Instance should have been started with manual reason")
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

		// Reset between tests
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

		// Call with manual restart - should restart even with Never policy
		err = controller.RestartInstance(context.Background(), instance, InstanceRestartReasonManual)
		assert.NoError(t, err)
		assert.Contains(t, testRunner.StoppedInstances, instance.ID, "Instance should have been stopped with manual reason")
		assert.Contains(t, testRunner.CreatedInstances, instance, "Instance should have been created with manual reason")
		assert.Contains(t, testRunner.StartedInstances, instance.ID, "Instance should have been started with manual reason")
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

	// Create a modified service with different image
	modifiedService := &types.Service{
		ID:            service.ID,
		Name:          service.Name,
		Namespace:     service.Namespace,
		RestartPolicy: service.RestartPolicy,
		Image:         "different-image:latest", // This should trigger incompatibility
		Runtime:       "docker",
		Metadata: &types.ServiceMetadata{
			Generation: 2, // Increment generation to trigger incompatibility
		},
	}

	// Update should fail due to incompatibility
	err = controller.UpdateInstance(ctx, modifiedService, instance)
	assert.Error(t, err, "UpdateInstance should return an error for incompatible changes")
	assert.Contains(t, err.Error(), "requires recreation", "Error should indicate recreation is needed")
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
