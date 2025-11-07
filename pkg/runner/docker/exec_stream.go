package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
)

// DockerClient defines the Docker API methods we need
type DockerClient interface {
	ContainerExecCreate(ctx context.Context, containerID string, config container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, config container.ExecStartOptions) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
	ContainerExecResize(ctx context.Context, execID string, options container.ResizeOptions) error
}

// DockerExecStream implements the runner.ExecStream interface for Docker containers.
type DockerExecStream struct {
	cli           DockerClient
	execID        string
	containerID   string
	instanceID    string
	logger        log.Logger
	hijackedResp  types.HijackedResponse
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	stdout        *readWritePipe
	stderr        *readWritePipe
	combined      io.Reader
	mutex         sync.Mutex
	tty           bool
	closed        bool
	exitCodeMutex sync.Mutex
	exitCode      int
	exitErr       error
}

// NewDockerExecStream creates a new DockerExecStream.
func NewDockerExecStream(
	ctx context.Context,
	cli DockerClient,
	containerID string,
	instanceID string,
	options runner.ExecOptions,
	logger log.Logger,
) (*DockerExecStream, error) {
	// Create cancellable context
	execCtx, cancel := context.WithCancel(ctx)

	// Prepare exec config - using the container package types
	config := container.ExecOptions{
		Cmd:          options.Command,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          options.TTY,
		Detach:       false,
	}

	// Add environment variables if provided
	if len(options.Env) > 0 {
		for k, v := range options.Env {
			config.Env = append(config.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Add working directory if provided
	if options.WorkingDir != "" {
		config.WorkingDir = options.WorkingDir
	}

	// Create exec instance
	logger.Debug("Creating exec instance",
		log.Str("containerId", containerID),
		log.Str("cmd", fmt.Sprintf("%v", options.Command)))
	resp, err := cli.ContainerExecCreate(execCtx, containerID, config)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create exec: %w", err)
	}

	// Connect to exec instance
	logger.Debug("Connecting to exec instance", log.Str("execId", resp.ID))
	attachResp, err := cli.ContainerExecAttach(execCtx, resp.ID, container.ExecStartOptions{
		Tty: options.TTY,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to attach to exec: %w", err)
	}

	var stdout, stderr *readWritePipe
	var combined io.Reader

	// Set up I/O streams
	if options.TTY {
		// With TTY, stdout and stderr are combined
		stdout = newReadWritePipe()
		stderr = newReadWritePipe() // Will be mostly unused with TTY
		combined = attachResp.Reader
	} else {
		// Without TTY, use stdcopy to demultiplex
		stdout = newReadWritePipe()
		stderr = newReadWritePipe()

		// We don't need to set combined as we'll handle demuxing explicitly
	}

	// Create the exec stream
	stream := &DockerExecStream{
		cli:          cli,
		execID:       resp.ID,
		containerID:  containerID,
		instanceID:   instanceID,
		logger:       logger.WithComponent("docker-exec-stream"),
		hijackedResp: attachResp,
		ctx:          execCtx,
		cancel:       cancel,
		stdout:       stdout,
		stderr:       stderr,
		combined:     combined,
		tty:          options.TTY,
	}

	// Start handling I/O
	stream.startIOCopy()

	// Set up terminal size if using TTY
	if options.TTY && options.TerminalWidth > 0 && options.TerminalHeight > 0 {
		if err := stream.ResizeTerminal(options.TerminalWidth, options.TerminalHeight); err != nil {
			logger.Warn("Failed to set initial terminal size", log.Err(err))
		}
	}

	return stream, nil
}

// Write sends data to the standard input of the process.
func (s *DockerExecStream) Write(p []byte) (n int, err error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closed {
		return 0, fmt.Errorf("exec stream is closed")
	}

	return s.hijackedResp.Conn.Write(p)
}

// Read reads data from the standard output of the process.
func (s *DockerExecStream) Read(p []byte) (n int, err error) {
	if s.closed {
		return 0, fmt.Errorf("exec stream is closed")
	}

	return s.stdout.Read(p)
}

// Stderr provides access to the standard error stream of the process.
func (s *DockerExecStream) Stderr() io.Reader {
	return s.stderr
}

// ResizeTerminal resizes the terminal (if TTY was enabled).
func (s *DockerExecStream) ResizeTerminal(width, height uint32) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closed {
		return fmt.Errorf("exec stream is closed")
	}

	if !s.tty {
		return fmt.Errorf("terminal resize requires TTY mode")
	}

	// Using the container package types
	return s.cli.ContainerExecResize(s.ctx, s.execID, container.ResizeOptions{
		Height: uint(height),
		Width:  uint(width),
	})
}

// Signal sends a signal to the process.
func (s *DockerExecStream) Signal(sigName string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closed {
		return fmt.Errorf("exec stream is closed")
	}

	// Docker doesn't have a direct way to send signals to exec processes
	// For SIGINT, we can send Ctrl+C if TTY is enabled
	if s.tty && strings.ToUpper(sigName) == "SIGINT" {
		_, err := s.hijackedResp.Conn.Write([]byte{3}) // Ctrl+C
		return err
	}

	// For other signals, we need to create a signal proxy in the container
	// This is a limitation of the Docker API
	s.logger.Warn("Signal not supported for Docker exec", log.Str("signal", sigName))
	return fmt.Errorf("sending signal %s not supported for Docker exec", sigName)
}

// ExitCode returns the exit code after the process has completed.
func (s *DockerExecStream) ExitCode() (int, error) {
	s.exitCodeMutex.Lock()
	defer s.exitCodeMutex.Unlock()

	if s.exitErr != nil {
		return s.exitCode, s.exitErr
	}

	// If exit code is already known, return it
	if s.exitCode != 0 || s.closed {
		return s.exitCode, nil
	}

	// Inspect the exec instance to get exit code
	resp, err := s.cli.ContainerExecInspect(s.ctx, s.execID)
	if err != nil {
		s.exitErr = fmt.Errorf("failed to inspect exec: %w", err)
		return 0, s.exitErr
	}

	// If the process is still running, return an error
	if resp.Running {
		return 0, fmt.Errorf("process is still running")
	}

	s.exitCode = resp.ExitCode
	return s.exitCode, nil
}

// Close terminates the exec session and releases resources.
func (s *DockerExecStream) Close() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.closed {
		return nil
	}

	// Mark as closed
	s.closed = true

	// Cancel context to stop all operations
	s.cancel()

	// Close hijacked connection
	s.hijackedResp.Close()

	// Wait for I/O goroutines to finish
	s.wg.Wait()

	// Close pipes
	s.stdout.Close()
	s.stderr.Close()

	return nil
}

// startIOCopy starts goroutines to handle I/O copying.
func (s *DockerExecStream) startIOCopy() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.hijackedResp.Close()
		}()

		if s.tty {
			// With TTY, stdout and stderr are combined
			_, err := io.Copy(s.stdout, s.combined)
			if err != nil && !isClosedError(err) {
				s.logger.Warn("Error copying output with TTY", log.Err(err))
			}
		} else {
			// Without TTY, demultiplex stdout and stderr
			_, err := stdcopy.StdCopy(s.stdout, s.stderr, s.hijackedResp.Reader)
			if err != nil && !isClosedError(err) {
				s.logger.Warn("Error copying demultiplexed output", log.Err(err))
			}
		}

		// Close the pipes to signal EOF to readers
		s.stdout.Close()
		s.stderr.Close()

		// After I/O is done, get exit code
		s.exitCodeMutex.Lock()
		defer s.exitCodeMutex.Unlock()

		resp, err := s.cli.ContainerExecInspect(context.Background(), s.execID)
		if err != nil {
			s.exitErr = fmt.Errorf("failed to inspect exec after completion: %w", err)
			return
		}
		s.exitCode = resp.ExitCode
	}()
}

// readWritePipe is a pipe that can be read from and written to.
type readWritePipe struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

// newReadWritePipe creates a new readWritePipe.
func newReadWritePipe() *readWritePipe {
	r, w := io.Pipe()
	return &readWritePipe{
		reader: r,
		writer: w,
	}
}

// Read reads from the pipe.
func (p *readWritePipe) Read(data []byte) (int, error) {
	return p.reader.Read(data)
}

// Write writes to the pipe.
func (p *readWritePipe) Write(data []byte) (int, error) {
	return p.writer.Write(data)
}

// Close closes both ends of the pipe.
func (p *readWritePipe) Close() error {
	err1 := p.reader.Close()
	err2 := p.writer.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// isClosedError checks if an error is due to a closed connection.
func isClosedError(err error) bool {
	if err == nil {
		return false
	}
	return err == io.EOF ||
		strings.Contains(err.Error(), "closed") ||
		strings.Contains(err.Error(), "EOF") ||
		err == io.ErrClosedPipe ||
		err == os.ErrClosed
}

// SliceStringer creates a string formatter for string slices.
type SliceStringer []string

// String implements the fmt.Stringer interface.
func (s SliceStringer) String() string {
	return fmt.Sprintf("%v", []string(s))
}
