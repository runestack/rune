package process

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewProcessExecStream(t *testing.T) {
	// Skip if running in CI environment
	if testing.Short() {
		t.Skip("Skipping test in short mode")
	}

	// Create a logger
	logger := log.NewLogger()

	// Create the exec options with a simple command
	options := runner.ExecOptions{
		Command: []string{"echo", "Hello, World!"},
		Env:     map[string]string{"TEST_VAR": "test_value"},
	}

	// Create the exec stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := NewProcessExecStream(ctx, "test-instance", options, logger)

	// Assert results
	assert.NoError(t, err)
	assert.NotNil(t, stream)

	// Read output
	buf := make([]byte, 100)
	n, err := stream.Read(buf)
	assert.NoError(t, err)
	assert.Greater(t, n, 0)
	assert.Contains(t, string(buf[:n]), "Hello, World!")

	// Close and cleanup
	err = stream.Close()
	assert.NoError(t, err)
}

// TestProcessExecStreamOutputSurvivesProcessExit is the regression
// test for output being lost when the process exits before the caller
// reads it.
//
// ProcessExecStream calls cmd.Wait in a background goroutine to track
// the exit code. When stdout came from cmd.StdoutPipe, that Wait
// closed the pipe the moment the process exited, so a short-lived
// command raced its own reader: Read returned "file already closed"
// and the output was gone. TestNewProcessExecStream hit this
// intermittently (reliably under GOMAXPROCS=1); here we wait for the
// process to exit *first*, so the read is exercised unconditionally.
func TestProcessExecStreamOutputSurvivesProcessExit(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping test in short mode")
	}

	logger := log.NewLogger()
	options := runner.ExecOptions{
		Command: []string{"sh", "-c", "echo 'Hello, World!'; echo 'Error message' >&2"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := NewProcessExecStream(ctx, "test-instance", options, logger)
	assert.NoError(t, err)
	defer stream.Close()

	// Wait for the process to fully exit — and with it, the background
	// cmd.Wait — before reading anything.
	require.Eventually(t, func() bool {
		_, err := stream.ExitCode()
		return err == nil
	}, 5*time.Second, 5*time.Millisecond, "process never reported an exit code")

	// Output written before exit must still be readable afterwards.
	buf := make([]byte, 100)
	n, err := stream.Read(buf)
	assert.NoError(t, err)
	assert.Contains(t, string(buf[:n]), "Hello, World!")

	errBuf := make([]byte, 100)
	n, err = stream.Stderr().Read(errBuf)
	assert.NoError(t, err)
	assert.Contains(t, string(errBuf[:n]), "Error message")
}

func TestProcessExecStreamExitCode(t *testing.T) {
	// Skip if running in CI environment
	if testing.Short() {
		t.Skip("Skipping test in short mode")
	}

	// Create a logger
	logger := log.NewLogger()

	// Create the exec options with a simple command that exits with code 0
	options := runner.ExecOptions{
		Command: []string{"true"},
	}

	// Create the exec stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := NewProcessExecStream(ctx, "test-instance", options, logger)
	assert.NoError(t, err)

	// Wait for the process to complete
	time.Sleep(100 * time.Millisecond)

	// Check exit code
	exitCode, err := stream.ExitCode()
	assert.NoError(t, err)
	assert.Equal(t, 0, exitCode)

	// Close and cleanup
	err = stream.Close()
	assert.NoError(t, err)
}

func TestProcessExecStreamExitCodeError(t *testing.T) {
	// Skip if running in CI environment
	if testing.Short() {
		t.Skip("Skipping test in short mode")
	}

	// Create a logger
	logger := log.NewLogger()

	// Create the exec options with a simple command that exits with non-zero code
	options := runner.ExecOptions{
		Command: []string{"false"},
	}

	// Create the exec stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := NewProcessExecStream(ctx, "test-instance", options, logger)
	assert.NoError(t, err)

	// Wait for the process to complete
	time.Sleep(100 * time.Millisecond)

	// Check exit code
	exitCode, err := stream.ExitCode()
	assert.NoError(t, err) // No error, but non-zero exit code
	assert.Equal(t, 1, exitCode)

	// Close and cleanup
	err = stream.Close()
	assert.NoError(t, err)
}

func TestProcessExecStreamSignal(t *testing.T) {
	// Skip if running in CI environment
	if testing.Short() {
		t.Skip("Skipping test in short mode")
	}

	// Create a logger
	logger := log.NewLogger()

	// Create the exec options with a command that sleeps
	options := runner.ExecOptions{
		Command: []string{"sleep", "10"},
	}

	// Create the exec stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := NewProcessExecStream(ctx, "test-instance", options, logger)
	assert.NoError(t, err)

	// Send SIGTERM to the process
	err = stream.Signal("SIGTERM")
	assert.NoError(t, err)

	// Wait a moment for the signal to be processed
	time.Sleep(100 * time.Millisecond)

	// The process should be terminated, so exit code should be available
	_, err = stream.ExitCode()
	assert.NoError(t, err) // No error because the process is done

	// Close and cleanup
	err = stream.Close()
	assert.NoError(t, err)
}

func TestProcessExecStreamStderr(t *testing.T) {
	// Skip if running in CI environment
	if testing.Short() {
		t.Skip("Skipping test in short mode")
	}

	// Create a logger
	logger := log.NewLogger()

	// Create the exec options with a command that writes to stderr
	options := runner.ExecOptions{
		Command: []string{"sh", "-c", "echo 'Error message' >&2"},
	}

	// Create the exec stream
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := NewProcessExecStream(ctx, "test-instance", options, logger)
	assert.NoError(t, err)

	// Get stderr reader
	stderrReader := stream.Stderr()
	assert.NotNil(t, stderrReader)

	// Read from stderr
	buf := make([]byte, 100)
	n, err := stderrReader.Read(buf)
	assert.NoError(t, err)
	assert.Greater(t, n, 0)
	assert.Contains(t, string(buf[:n]), "Error message")

	// Wait for the process to complete
	time.Sleep(100 * time.Millisecond)

	// Close and cleanup
	err = stream.Close()
	assert.NoError(t, err)
}
