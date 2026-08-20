package process

import (
	"context"
	"io"
	"strconv"
	"strings"
	"syscall"
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

	// ...and once drained, the stream must still end. Nothing inherited
	// these descriptors from the command, so the child was the last
	// writer and its exit is a real EOF.
	_, err = stream.Read(buf)
	assert.ErrorIs(t, err, io.EOF)
}

// TestChildExitReleasesParkedStdinWrite pins the other half of pipe
// ownership: releasing the parent's stdin write end when the child exits.
//
// exec.Cmd used to do this for us — cmd.Wait closes the pipes it owns,
// which unparked any Write blocked on a full stdin pipe. Owning the pipes
// ourselves means we have to do it, and the first cut of that change did
// not: a parked Write held s.mutex forever, and Close, which needs the
// mutex before it can cancel and clean up, wedged behind it.
//
// Note what this does and does not pin. Child *exit* is what releases the
// write; Close does not, and cannot — it is stuck behind the same mutex.
// A live child that simply never drains stdin still wedges Close, on this
// code and on the code before it. That is a separate defect.
func TestChildExitReleasesParkedStdinWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping test in short mode")
	}

	logger := log.NewLogger()
	// The child hands stdin to a grandchild and exits, so the pipe stays
	// open and the parked write cannot drain. It reports the grandchild's
	// pid on stdout so we can reap it — CommandContext kills only the
	// direct child, which by then is already gone. The sleep before exit
	// gives the writer below room to park first.
	options := runner.ExecOptions{
		Command: []string{"sh", "-c", "exec 3<&0; sleep 10 0<&3 & echo $!; sleep 1; exit 0"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := NewProcessExecStream(ctx, "test-instance", options, logger)
	require.NoError(t, err)
	defer stream.Close()

	pidBuf := make([]byte, 32)
	n, err := stream.Read(pidBuf)
	require.NoError(t, err)
	grandchild, err := strconv.Atoi(strings.TrimSpace(string(pidBuf[:n])))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	// Park a write larger than the pipe buffer, so it blocks holding the
	// stream mutex.
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		_, _ = stream.Write(make([]byte, 1<<20))
	}()

	// Wait until the write genuinely holds the mutex. Without this the
	// assertions below could pass merely because the writer had not
	// started yet — which they do, even with the fix reverted.
	require.Eventually(t, func() bool {
		if stream.mutex.TryLock() {
			stream.mutex.Unlock()
			return false
		}
		return true
	}, 5*time.Second, time.Millisecond, "write never parked holding the stream mutex")

	// The child's exit must release the parked write. This is the
	// assertion that actually pins the fix.
	select {
	case <-writeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("parked stdin write was never released after the child exited")
	}

	// And Close must not wedge behind it.
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		_ = stream.Close()
	}()

	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close wedged behind a write parked on a full stdin pipe")
	}
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
