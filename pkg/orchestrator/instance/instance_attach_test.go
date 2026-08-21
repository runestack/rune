package instance

import (
	"io"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
