package client

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/utils"
	"golang.org/x/term"
	"google.golang.org/grpc/codes"
)

// ExecClient provides methods for executing commands in instances.
type ExecClient struct {
	client *Client
	logger log.Logger
	exec   generated.ExecServiceClient
}

// NewExecClient creates a new exec client.
func NewExecClient(client *Client) *ExecClient {
	return &ExecClient{
		client: client,
		logger: client.logger.WithComponent("exec-client"),
		exec:   generated.NewExecServiceClient(client.conn),
	}
}

// ExecOptions defines the configuration for command execution.
type ExecOptions struct {
	Command        []string          // Command and arguments to execute
	Env            map[string]string // Environment variables to set
	WorkingDir     string            // Working directory for the command
	TTY            bool              // Whether to allocate a pseudo-TTY
	TerminalWidth  uint32            // Initial terminal width
	TerminalHeight uint32            // Initial terminal height
	Timeout        time.Duration     // Timeout for the exec session
}

// ExecSession represents an active exec session.
type ExecSession struct {
	stream    generated.ExecService_StreamExecClient
	client    *ExecClient
	logger    log.Logger
	exitState int // Track exit command state
}

// NewExecSession creates a new exec session.
func (e *ExecClient) NewExecSession(ctx context.Context) (*ExecSession, error) {
	stream, err := e.exec.StreamExec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create exec stream: %w", err)
	}

	return &ExecSession{
		stream: stream,
		client: e,
		logger: e.logger.WithComponent("exec-session"),
	}, nil
}

// InitializeInstanceTarget initializes the exec session with the given options.
func (s *ExecSession) InitializeInstanceTarget(target string, namespace string, options *ExecOptions) error {
	initReq := &generated.ExecInitRequest{
		Target: &generated.ExecInitRequest_InstanceId{
			InstanceId: target,
		},
		Namespace:  namespace,
		Command:    options.Command,
		Env:        options.Env,
		WorkingDir: options.WorkingDir,
		Tty:        options.TTY,
	}

	// Set terminal size if TTY is enabled
	if options.TTY && (options.TerminalWidth > 0 || options.TerminalHeight > 0) {
		initReq.TerminalSize = &generated.TerminalSize{
			Width:  options.TerminalWidth,
			Height: options.TerminalHeight,
		}
	}

	// Send initialization request
	req := &generated.ExecRequest{
		Request: &generated.ExecRequest_Init{
			Init: initReq,
		},
	}

	if err := s.stream.Send(req); err != nil {
		return fmt.Errorf("failed to send exec init request: %w", err)
	}

	return nil
}

// InitializeServiceTarget initializes the exec session with the given options.
func (s *ExecSession) InitializeServiceTarget(target string, namespace string, options *ExecOptions) error {
	initReq := &generated.ExecInitRequest{
		Target: &generated.ExecInitRequest_ServiceName{
			ServiceName: target,
		},
		Namespace:  namespace,
		Command:    options.Command,
		Env:        options.Env,
		WorkingDir: options.WorkingDir,
		Tty:        options.TTY,
	}

	// Set terminal size if TTY is enabled
	if options.TTY && (options.TerminalWidth > 0 || options.TerminalHeight > 0) {
		initReq.TerminalSize = &generated.TerminalSize{
			Width:  options.TerminalWidth,
			Height: options.TerminalHeight,
		}
	}

	// Send initialization request
	req := &generated.ExecRequest{
		Request: &generated.ExecRequest_Init{
			Init: initReq,
		},
	}

	if err := s.stream.Send(req); err != nil {
		return fmt.Errorf("failed to send exec init request: %w", err)
	}

	return nil
}

// RunInteractive runs an interactive exec session.
func (s *ExecSession) RunInteractive() error {
	// Get terminal state
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to make terminal raw: %w", err)
	}

	// Ensure terminal is restored when we exit
	defer func() {
		term.Restore(int(os.Stdin.Fd()), oldState)
	}()

	// Set up signal handling
	// IMPORTANT: In TTY mode we should NOT translate Ctrl+C into SIGINT here.
	// Docker's behavior for `exec -it` is to pass raw 0x03 (ETX) bytes through
	// the PTY and let the remote shell decide what to do. So we only watch for
	// terminal resize (SIGWINCH) in interactive mode.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGWINCH)
	defer signal.Stop(sigChan)

	// Start goroutines for I/O handling
	errChan := make(chan error, 2)
	doneChan := make(chan struct{})

	// Handle stdin
	go func() {
		buffer := make([]byte, 1024)
		for {
			select {
			case <-doneChan:
				return
			default:
			}

			n, err := os.Stdin.Read(buffer)
			if err != nil {
				if err == io.EOF {
					return
				}
				errChan <- fmt.Errorf("stdin read error: %w", err)
				return
			}

			if n > 0 {
				input := string(buffer[:n])

				// Check if this is an exit command or Ctrl+D
				if s.isExitCommand(input) {
					// Send the input first, then close
					req := &generated.ExecRequest{
						Request: &generated.ExecRequest_Stdin{
							Stdin: buffer[:n],
						},
					}
					if err := s.stream.Send(req); err != nil {
						errChan <- fmt.Errorf("failed to send stdin: %w", err)
						return
					}

					// Wait a moment for the server to process, then close
					time.Sleep(100 * time.Millisecond)
					close(doneChan)
					errChan <- nil
					return
				}

				req := &generated.ExecRequest{
					Request: &generated.ExecRequest_Stdin{
						Stdin: buffer[:n],
					},
				}

				if err := s.stream.Send(req); err != nil {
					errChan <- fmt.Errorf("failed to send stdin: %w", err)
					return
				}
			}
		}
	}()

	// Handle stdout/stderr and responses
	go func() {
		for {
			resp, err := s.stream.Recv()
			if err != nil {
				s.logger.Debug("CLIENT: Stream recv error", log.Err(err), log.Bool("isEOF", err == io.EOF), log.Bool("isBenign", isBenignStreamError(err)))
				if err == io.EOF || isBenignStreamError(err) {
					// graceful end of stream or benign transport close
					s.logger.Debug("CLIENT: Stream ended gracefully")
					close(doneChan) // Signal stdin to stop
					errChan <- nil
					return
				}
				errChan <- fmt.Errorf("stream recv error: %w", err)
				return
			}

			// Check if this is an exit response
			if exitResp := resp.GetExit(); exitResp != nil {
				s.logger.Debug("CLIENT: Received exit response", log.Int("code", int(exitResp.Code)), log.Bool("signaled", exitResp.Signaled))
				// Signal stdin goroutine to stop
				close(doneChan)
				if exitResp.Signaled {
					errChan <- fmt.Errorf("process exited due to signal: %s", exitResp.Signal)
				} else {
					// Any exit code (including non-zero) should be treated as successful completion
					s.logger.Debug("CLIENT: Process exited with code, treating as success", log.Int("code", int(exitResp.Code)))
					errChan <- nil
				}
				return
			}

			// Handle other response types
			switch r := resp.GetResponse().(type) {
			case *generated.ExecResponse_Stdout:
				s.logger.Debug("CLIENT: Received stdout", log.Int("bytes", len(r.Stdout)))
				if err := s.handleResponse(resp); err != nil {
					errChan <- err
					return
				}
			case *generated.ExecResponse_Stderr:
				s.logger.Debug("CLIENT: Received stderr", log.Int("bytes", len(r.Stderr)))
				if err := s.handleResponse(resp); err != nil {
					errChan <- err
					return
				}
			default:
				s.logger.Debug("CLIENT: Received other response type")
				if err := s.handleResponse(resp); err != nil {
					errChan <- err
					return
				}
			}
		}
	}()

	// Handle signals
	go func() {
		for sig := range sigChan {
			switch sig {
			case syscall.SIGWINCH:
				// Handle terminal resize
				if width, height, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
					req := &generated.ExecRequest{
						Request: &generated.ExecRequest_Resize{
							Resize: &generated.TerminalSize{
								Width:  utils.ToUint32NonNegative(width),
								Height: utils.ToUint32NonNegative(height),
							},
						},
					}
					if err := s.stream.Send(req); err != nil {
						errChan <- fmt.Errorf("failed to send resize: %w", err)
						return
					}
				}
			}
		}
	}()

	// Wait for completion or error
	err = <-errChan

	// Close stdin stream to signal server that we're done
	if closeErr := s.stream.CloseSend(); closeErr != nil {
		s.logger.Debug("CLIENT: Error closing stream", log.Err(closeErr))
	}

	return err
}

// isExitCommand checks if the input represents an exit command or Ctrl+D
func (s *ExecSession) isExitCommand(input string) bool {
	// Check for Ctrl+D (EOF character)
	if input == "\x04" {
		return true
	}

	// State machine to detect "exit" followed by newline
	if input == "e" {
		s.exitState = 1
	} else if input == "x" && s.exitState == 1 {
		s.exitState = 2
	} else if input == "i" && s.exitState == 2 {
		s.exitState = 3
	} else if input == "t" && s.exitState == 3 {
		s.exitState = 4
	} else if (input == "\n" || input == "\r") && s.exitState == 4 {
		s.exitState = 0 // Reset state
		return true
	} else {
		s.exitState = 0 // Reset state on any other input
	}

	return false
}

// RunNonInteractive runs a non-interactive exec session.
func (s *ExecSession) RunNonInteractive() error {
	// Handle responses
	for {
		resp, err := s.stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("stream recv error: %w", err)
		}

		// Check if this is an exit response
		if exitResp := resp.GetExit(); exitResp != nil {
			// Process has exited - return to terminate the session
			if exitResp.Signaled {
				return fmt.Errorf("process exited due to signal: %s", exitResp.Signal)
			}
			// For non-interactive sessions, treat any exit as successful completion
			return nil
		}

		// Handle other responses
		if err := s.handleResponse(resp); err != nil {
			return err
		}
	}
}

// handleResponse handles a response from the exec stream.
func (s *ExecSession) handleResponse(resp *generated.ExecResponse) error {
	switch r := resp.GetResponse().(type) {
	case *generated.ExecResponse_Stdout:
		os.Stdout.Write(r.Stdout)
	case *generated.ExecResponse_Stderr:
		os.Stderr.Write(r.Stderr)
	case *generated.ExecResponse_Status:
		if r.Status.Code != int32(codes.OK) {
			return fmt.Errorf("exec error: %s", r.Status.Message)
		}
	}
	return nil
}

// Close closes the exec session.
func (s *ExecSession) Close() error {
	// Best-effort close; ignore benign errors
	if err := s.stream.CloseSend(); err != nil {
		if isBenignStreamError(err) {
			return nil
		}
		return err
	}
	return nil
}

// isBenignStreamError detects transport-layer errors that happen on normal closure
func isBenignStreamError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Docker hijacked connection often reports use of closed network connection on normal close
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(strings.ToLower(msg), "client disconnected") ||
		strings.Contains(strings.ToLower(msg), "broken pipe")
}
