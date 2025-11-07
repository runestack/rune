package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
)

// ProcessExecStream implements the runner.ExecStream interface for processes.
type ProcessExecStream struct {
	ctx           context.Context
	cancel        context.CancelFunc
	cmd           *exec.Cmd
	instanceID    string
	logger        log.Logger
	stdin         io.WriteCloser
	stdout        io.ReadCloser
	stderr        io.ReadCloser
	mutex         sync.Mutex
	closed        bool
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

	// Set up I/O streams
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Create the exec stream
	stream := &ProcessExecStream{
		ctx:        execCtx,
		cancel:     cancel,
		cmd:        cmd,
		instanceID: instanceID,
		logger:     logger.WithComponent("process-exec-stream"),
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		doneCh:     make(chan struct{}),
	}

	// Start the command
	logger.Debug("Starting command",
		log.Str("instanceId", instanceID),
		log.Str("cmd", fmt.Sprintf("%v", options.Command)))

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start command: %w", err)
	}

	// Start a goroutine to wait for command completion
	stream.wg.Add(1)
	go func() {
		defer stream.wg.Done()
		defer close(stream.doneCh)

		err := cmd.Wait()

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

	if s.closed {
		return 0, fmt.Errorf("exec stream is closed")
	}

	return s.stdin.Write(p)
}

// Read reads data from the standard output of the process.
func (s *ProcessExecStream) Read(p []byte) (n int, err error) {
	if s.closed {
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

	if s.closed {
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

	if s.closed {
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

	if s.closed {
		return nil
	}

	// Mark as closed
	s.closed = true

	// Cancel context to stop the command
	s.cancel()

	// Close I/O pipes
	if s.stdin != nil {
		s.stdin.Close()
	}

	// We don't close stdout/stderr here as they're automatically closed when the process terminates

	// Wait for all goroutines to finish
	s.wg.Wait()

	return nil
}
