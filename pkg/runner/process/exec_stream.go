package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
)

// ProcessExecStream implements the runner.ExecStream interface for processes.
//
// Callers must Close the stream when done. Its pipes are owned by the
// stream rather than by exec.Cmd, so — unlike a cmd.StdoutPipe stream —
// the stdout and stderr read ends are not released implicitly when the
// process exits. The stdin write end is: the wait goroutine closes it on
// exit, so a writer parked on a full pipe cannot wedge Close.
type ProcessExecStream struct {
	ctx           context.Context
	cancel        context.CancelFunc
	cmd           *exec.Cmd
	instanceID    string
	logger        log.Logger
	stdin         *os.File
	stdout        *os.File
	stderr        *os.File
	mutex         sync.Mutex
	closed        atomic.Bool
	exitCodeMutex sync.Mutex
	exitCode      int
	exitErr       error
	wg            sync.WaitGroup
	doneCh        chan struct{}
}

// NewProcessExecStream creates a new ProcessExecStream.
func NewProcessExecStream(
	ctx context.Context,
	instanceID string,
	options runner.ExecOptions,
	logger log.Logger,
) (*ProcessExecStream, error) {
	// Create cancellable context
	execCtx, cancel := context.WithCancel(ctx)

	// Create command
	cmd := exec.CommandContext(execCtx, options.Command[0], options.Command[1:]...)

	// Set up environment variables
	if len(options.Env) > 0 {
		env := os.Environ() // Start with current environment
		for k, v := range options.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
	}

	// Set working directory if provided
	if options.WorkingDir != "" {
		cmd.Dir = options.WorkingDir
	}

	// Set up I/O streams.
	//
	// We deliberately do NOT use cmd.StdinPipe/StdoutPipe/StderrPipe.
	// Those pipes belong to exec.Cmd, and cmd.Wait closes them as soon
	// as the process exits. We call cmd.Wait from a background
	// goroutine (to track the exit code), so with cmd-owned pipes a
	// short-lived command could have its output closed out from under
	// the caller before they ever got to Read it — the reader would
	// see "file already closed" instead of the output. Pipes we make
	// ourselves are not owned by exec.Cmd: the data stays buffered and
	// readable until the caller drains it or calls Close.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		cancel()
		closeFiles(stdinR, stdinW)
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		cancel()
		closeFiles(stdinR, stdinW, stdoutR, stdoutW)
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	// Create the exec stream
	stream := &ProcessExecStream{
		ctx:        execCtx,
		cancel:     cancel,
		cmd:        cmd,
		instanceID: instanceID,
		logger:     logger.WithComponent("process-exec-stream"),
		stdin:      stdinW,
		stdout:     stdoutR,
		stderr:     stderrR,
		doneCh:     make(chan struct{}),
	}

	// Start the command
	logger.Debug("Starting command",
		log.Str("instanceId", instanceID),
		log.Str("cmd", fmt.Sprintf("%v", options.Command)))

	startErr := cmd.Start()

	// The child has inherited its ends of the pipes, so drop the
	// parent's copies: that leaves the child (and anything that
	// inherits these descriptors from it) as the only writers. Keeping
	// them open here would make the parent a writer too, and stdout and
	// stderr would never reach EOF at all — EOF arrives once the last
	// writer closes, so a backgrounded grandchild still holds it off.
	closeFiles(stdinR, stdoutW, stderrW)

	if startErr != nil {
		cancel()
		closeFiles(stdinW, stdoutR, stderrR)
		return nil, fmt.Errorf("failed to start command: %w", startErr)
	}

	// Start a goroutine to wait for command completion
	stream.wg.Add(1)
	go func() {
		defer stream.wg.Done()
		defer close(stream.doneCh)

		err := cmd.Wait()

		// Mirror what exec.Cmd does with its own pipes: release the
		// parent's stdin write end as soon as the child is gone.
		// Without this, a Write parked on a full stdin pipe would stay
		// parked forever holding s.mutex, and Close — which needs that
		// mutex before it can cancel and clean up — would wedge behind
		// it for the life of the process.
		closeFiles(stream.stdin)

		stream.exitCodeMutex.Lock()
		defer stream.exitCodeMutex.Unlock()

		if err != nil {
			stream.exitErr = err
			if exitErr, ok := err.(*exec.ExitError); ok {
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
					stream.exitCode = status.ExitStatus()
					return
				}
			}
			stream.exitCode = 1 // Default to 1 for errors
		} else {
			stream.exitCode = 0
		}
	}()

	return stream, nil
}

// Write sends data to the standard input of the process.
func (s *ProcessExecStream) Write(p []byte) (n int, err error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closed.Load() {
		return 0, fmt.Errorf("exec stream is closed")
	}

	return s.stdin.Write(p)
}

// Read reads data from the standard output of the process.
func (s *ProcessExecStream) Read(p []byte) (n int, err error) {
	if s.closed.Load() {
		return 0, fmt.Errorf("exec stream is closed")
	}

	return s.stdout.Read(p)
}

// Stderr provides access to the standard error stream of the process.
func (s *ProcessExecStream) Stderr() io.Reader {
	return s.stderr
}

// ResizeTerminal resizes the terminal (if TTY was enabled).
// For processes, terminal resizing isn't typically supported through the Go API.
func (s *ProcessExecStream) ResizeTerminal(width, height uint32) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closed.Load() {
		return fmt.Errorf("exec stream is closed")
	}

	// This is a stub - process TTY resizing requires additional platform-specific code
	s.logger.Warn("Terminal resize not supported for processes")
	return fmt.Errorf("terminal resize not supported for processes")
}

// Signal sends a signal to the process.
func (s *ProcessExecStream) Signal(sigName string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closed.Load() {
		return fmt.Errorf("exec stream is closed")
	}

	// Map signal name to syscall signal
	var sig syscall.Signal
	switch sigName {
	case "SIGINT":
		sig = syscall.SIGINT
	case "SIGTERM":
		sig = syscall.SIGTERM
	case "SIGKILL":
		sig = syscall.SIGKILL
	case "SIGHUP":
		sig = syscall.SIGHUP
	default:
		return fmt.Errorf("unknown signal: %s", sigName)
	}

	if s.cmd == nil || s.cmd.Process == nil {
		return fmt.Errorf("process not started")
	}

	return s.cmd.Process.Signal(sig)
}

// ExitCode returns the exit code after the process has completed.
func (s *ProcessExecStream) ExitCode() (int, error) {
	s.exitCodeMutex.Lock()
	defer s.exitCodeMutex.Unlock()

	// If there's an error, return it
	if s.exitErr != nil && s.exitCode == 0 {
		return s.exitCode, s.exitErr
	}

	// If the process is still running, check
	select {
	case <-s.doneCh:
		// Process has completed
		return s.exitCode, nil
	default:
		// Process is still running
		return 0, fmt.Errorf("process is still running")
	}
}

// Close terminates the exec session and releases resources.
func (s *ProcessExecStream) Close() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closed.Load() {
		return nil
	}

	// Mark as closed
	s.closed.Store(true)

	// Cancel context to stop the command
	s.cancel()

	// Close I/O pipes
	if s.stdin != nil {
		s.stdin.Close()
	}

	// Wait for all goroutines to finish
	s.wg.Wait()

	// The read ends are ours, not exec.Cmd's, so we release them here.
	// Any reader still blocked in Read is unblocked by this.
	closeFiles(s.stdout, s.stderr)

	return nil
}

// closeFiles closes every non-nil file, ignoring errors. Used for
// cleaning up pipe ends on setup failures and on Close.
func closeFiles(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			f.Close()
		}
	}
}
