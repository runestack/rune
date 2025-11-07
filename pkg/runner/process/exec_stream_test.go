package process

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/stretchr/testify/assert"
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
