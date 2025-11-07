package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExecService implements the gRPC ExecService.
type ExecService struct {
	generated.UnimplementedExecServiceServer

	logger       log.Logger
	orchestrator orchestrator.Orchestrator
}

// NewExecService creates a new ExecService with the given runners, store, and logger.
func NewExecService(logger log.Logger, orchestrator orchestrator.Orchestrator) *ExecService {
	return &ExecService{
		logger:       logger.WithComponent("exec-service"),
		orchestrator: orchestrator,
	}
}

// getTargetInstance resolves an instance from an init request, handling both service-based and instance-based targeting
func (s *ExecService) getTargetInstance(ctx context.Context, initReq *generated.ExecInitRequest) (*types.Instance, error) {
	namespace := types.NS(initReq.Namespace)

	if initReq.GetServiceName() != "" {
		s.logger.Debug("Getting instance from service", log.Str("service", initReq.GetServiceName()))
		return s.getInstanceFromService(ctx, namespace, initReq.GetServiceName())
	}

	return s.getInstanceByID(ctx, namespace, initReq.GetInstanceId())
}

// getInstanceFromService finds a running instance for the given service
func (s *ExecService) getInstanceFromService(ctx context.Context, namespace, serviceName string) (*types.Instance, error) {
	// Get running instances for the service
	instances, err := s.orchestrator.ListRunningInstances(ctx, namespace)
	if err != nil {
		s.logger.Error("Failed to list running instances from orchestrator", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to list running instances: %v", err)
	}

	s.logger.Debug("Found running instances",
		log.Int("count", len(instances)),
		log.Str("service", serviceName),
		log.Str("namespace", namespace))

	// Find the first running instance for this service
	for _, instance := range instances {
		s.logger.Debug("Checking instance",
			log.Str("id", instance.ID),
			log.Str("serviceName", instance.ServiceName),
			log.Str("serviceID", instance.ServiceID),
			log.Str("status", string(instance.Status)))

		if instance.ServiceName == serviceName {
			s.logger.Debug("Found matching instance", log.Str("id", instance.ID))
			return instance, nil
		}
	}

	s.logger.Warn("No running instances found for service",
		log.Str("service", serviceName),
		log.Str("namespace", namespace),
		log.Int("total_instances", len(instances)))

	return nil, status.Errorf(codes.NotFound, "no running instances found for service: %s in namespace: %s", serviceName, namespace)
}

// getInstanceByID retrieves an instance by ID and validates it's running
func (s *ExecService) getInstanceByID(ctx context.Context, namespace, instanceID string) (*types.Instance, error) {
	// Get instance from store first
	instance, err := s.orchestrator.GetInstanceByID(ctx, namespace, instanceID)
	if err != nil {
		if IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "instance not found: %s", instanceID)
		}
		s.logger.Error("Failed to get instance", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to get instance: %v", err)
	}

	// Get current status from orchestrator to ensure we have the latest status
	statusInfo, err := s.orchestrator.GetInstanceStatus(ctx, namespace, instanceID)
	if err != nil {
		s.logger.Warn("Failed to get instance status from orchestrator, using store status", log.Err(err))
		// Continue with store status if orchestrator fails
	} else {
		// Update instance with current status
		instance.Status = statusInfo.Status
	}

	if instance.Status != types.InstanceStatusRunning {
		return nil, status.Errorf(codes.FailedPrecondition, "instance is not running, status: %s", instance.Status)
	}

	return instance, nil
}

// StreamExec provides bidirectional streaming for exec operations.
func (s *ExecService) StreamExec(stream generated.ExecService_StreamExecServer) error {
	// Initialize the exec session
	execStream, instanceID, err := s.initializeExecSession(stream)
	if err != nil {
		return err
	}
	defer execStream.Close()

	// Send connection status
	if err := s.sendConnectionStatus(stream, instanceID); err != nil {
		return err
	}

	// Run the exec session
	return s.runExecSession(stream, execStream)
}

// initializeExecSession sets up the exec session and returns the exec stream and instance ID
func (s *ExecService) initializeExecSession(stream generated.ExecService_StreamExecServer) (types.ExecStream, string, error) {
	// Get and validate the initial request
	initReq, err := s.getInitialRequest(stream)
	if err != nil {
		return nil, "", err
	}

	// Set up context with cancel
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	// Get target instance
	instance, err := s.getTargetInstance(ctx, initReq)
	if err != nil {
		return nil, "", err
	}

	// Create exec options
	execOptions := s.createExecOptions(initReq)

	// Create exec stream
	execStream, err := s.createExecStream(ctx, initReq, instance, execOptions)
	if err != nil {
		return nil, "", err
	}

	return execStream, instance.ID, nil
}

// getInitialRequest receives and validates the initial exec request
func (s *ExecService) getInitialRequest(stream generated.ExecService_StreamExecServer) (*generated.ExecInitRequest, error) {
	req, err := stream.Recv()
	if err != nil {
		s.logger.Error("Failed to receive initial exec request", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to receive initial request: %v", err)
	}

	if err := s.validateExecRequest(req); err != nil {
		s.logger.Error("Invalid exec request", log.Err(err))
		return nil, status.Errorf(codes.InvalidArgument, "invalid exec request: %v", err)
	}

	initReq := req.GetInit()
	if initReq == nil {
		return nil, status.Errorf(codes.InvalidArgument, "first request must be an init request")
	}

	return initReq, nil
}

// createExecOptions converts gRPC options to orchestrator options
func (s *ExecService) createExecOptions(initReq *generated.ExecInitRequest) types.ExecOptions {
	return types.ExecOptions{
		Command:        initReq.Command,
		Env:            initReq.Env,
		WorkingDir:     initReq.WorkingDir,
		TTY:            initReq.Tty,
		TerminalWidth:  initReq.TerminalSize.GetWidth(),
		TerminalHeight: initReq.TerminalSize.GetHeight(),
	}
}

// createExecStream creates the exec stream based on the target type
func (s *ExecService) createExecStream(ctx context.Context, initReq *generated.ExecInitRequest, instance *types.Instance, execOptions types.ExecOptions) (types.ExecStream, error) {
	namespace := types.NS(initReq.Namespace)

	if initReq.GetServiceName() != "" {
		// Target is a service - use ExecInService
		serviceName := initReq.GetServiceName()
		execStream, err := s.orchestrator.ExecInService(ctx, namespace, serviceName, execOptions)
		if err != nil {
			s.logger.Error("Failed to exec in service", log.Str("service", serviceName), log.Err(err))
			return nil, status.Errorf(codes.Internal, "failed to exec in service: %v", err)
		}
		return execStream, nil
	}

	// Target is a specific instance
	execStream, err := s.orchestrator.ExecInInstance(ctx, namespace, instance.ID, execOptions)
	if err != nil {
		s.logger.Error("Failed to exec in instance", log.Str("instance", instance.ID), log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to exec in instance: %v", err)
	}

	return execStream, nil
}

// sendConnectionStatus sends the initial connection status to the client
func (s *ExecService) sendConnectionStatus(stream generated.ExecService_StreamExecServer, instanceID string) error {
	return stream.Send(&generated.ExecResponse{
		Response: &generated.ExecResponse_Status{
			Status: &generated.Status{
				Code:    int32(codes.OK),
				Message: fmt.Sprintf("Connected to instance %s", instanceID),
			},
		},
	})
}

// runExecSession handles the bidirectional streaming of the exec session
func (s *ExecService) runExecSession(stream generated.ExecService_StreamExecServer, execStream types.ExecStream) error {
	ctx := stream.Context()

	// Set up channels for coordination
	doneCh := make(chan error, 1)
	errorCh := make(chan error, 2)

	// Start I/O handlers
	s.startIOHandlers(stream, execStream, doneCh, errorCh, ctx)

	// Wait for completion
	return s.waitForCompletion(doneCh, errorCh, ctx, stream, execStream)
}

// startIOHandlers starts the stdin and stdout handlers
func (s *ExecService) startIOHandlers(stream generated.ExecService_StreamExecServer, execStream types.ExecStream, doneCh chan<- error, errorCh chan<- error, ctx context.Context) {
	// Start stdin handler
	go s.handleStdin(stream, execStream, doneCh, errorCh, ctx)

	// Start stdout handler
	go s.handleStdout(stream, execStream, errorCh, ctx)

	// Start stderr handler (best-effort)
	go s.handleStderr(stream, execStream.Stderr(), ctx)
}

// handleStdin processes stdin requests from the client
func (s *ExecService) handleStdin(stream generated.ExecService_StreamExecServer, execStream types.ExecStream, doneCh chan<- error, errorCh chan<- error, ctx context.Context) {
	defer func() {
		doneCh <- nil
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			// Treat client cancellations as benign
			if sErr, ok := status.FromError(err); ok && (sErr.Code() == codes.Canceled || sErr.Code() == codes.DeadlineExceeded) {
				s.logger.Debug("Exec stdin stream closed by client", log.Err(err))
				return
			}
			s.logger.Warn("Failed to receive exec request", log.Err(err))
			errorCh <- fmt.Errorf("failed to receive exec request: %w", err)
			return
		}

		if err := s.processRequest(req, execStream); err != nil {
			errorCh <- err
			return
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// processRequest handles different types of exec requests
func (s *ExecService) processRequest(req *generated.ExecRequest, execStream types.ExecStream) error {
	switch {
	case req.GetStdin() != nil:
		return s.handleStdinData(execStream, req.GetStdin())
	case req.GetResize() != nil:
		return s.handleResize(execStream, req.GetResize())
	case req.GetSignal() != nil:
		return s.handleSignal(execStream, req.GetSignal())
	default:
		s.logger.Warn("Received unknown exec request type")
		return nil
	}
}

// handleStdinData writes stdin data to the exec stream
func (s *ExecService) handleStdinData(execStream types.ExecStream, data []byte) error {
	_, err := execStream.Write(data)
	if err != nil && !isBenignStreamError(err) {
		s.logger.Error("Failed to write to exec stream", log.Err(err))
		return fmt.Errorf("failed to write to exec stream: %w", err)
	}
	return nil
}

// handleResize resizes the terminal
func (s *ExecService) handleResize(execStream types.ExecStream, resize *generated.TerminalSize) error {
	if err := execStream.ResizeTerminal(resize.Width, resize.Height); err != nil {
		s.logger.Warn("Failed to resize terminal", log.Err(err))
	}
	return nil
}

// handleSignal sends a signal to the process
func (s *ExecService) handleSignal(execStream types.ExecStream, signal *generated.Signal) error {
	if err := execStream.Signal(signal.Name); err != nil {
		s.logger.Warn("Failed to send signal", log.Str("signal", signal.Name), log.Err(err))
	}
	return nil
}

// handleStdout reads from the exec stream and sends to the client
func (s *ExecService) handleStdout(stream generated.ExecService_StreamExecServer, execStream types.ExecStream, errorCh chan<- error, ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("Panic in stdout handler", log.Any("panic", r))
		}
	}()

	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := execStream.Read(buf)
		if err == io.EOF {
			// Signal completion when we reach EOF
			select {
			case errorCh <- nil:
			default:
			}
			return
		}
		if err != nil {
			if isBenignStreamError(err) {
				// For benign errors, still signal completion
				select {
				case errorCh <- nil:
				default:
				}
				return
			}
			s.logger.Error("Failed to read from exec stream", log.Err(err))
			errorCh <- fmt.Errorf("failed to read from exec stream: %w", err)
			return
		}

		if n > 0 {
			// Send stdout
			if err := stream.Send(&generated.ExecResponse{Response: &generated.ExecResponse_Stdout{Stdout: buf[:n]}}); err != nil {
				// Client closed stream; treat as benign
				if sErr, ok := status.FromError(err); ok && (sErr.Code() == codes.Canceled || sErr.Code() == codes.DeadlineExceeded) {
					s.logger.Debug("Exec stdout stream closed by client", log.Err(err))
					return
				}
				s.logger.Warn("Failed to send stdout", log.Err(err))
				errorCh <- fmt.Errorf("failed to send stdout: %w", err)
				return
			}
		}
	}
}

// handleStderr reads from the exec stderr stream and sends to the client (best-effort)
func (s *ExecService) handleStderr(stream generated.ExecService_StreamExecServer, r io.Reader, ctx context.Context) {
	if r == nil {
		return
	}
	buf := make([]byte, 4096)
	for {
		// Note: this read may block; acceptable for test and simple implementation
		n, err := r.Read(buf)
		if n > 0 {
			if err := stream.Send(&generated.ExecResponse{Response: &generated.ExecResponse_Stderr{Stderr: buf[:n]}}); err != nil {
				s.logger.Warn("Failed to send stderr", log.Err(err))
				return
			}
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			// Best-effort: log and stop
			s.logger.Debug("stderr read ended", log.Err(err))
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// waitForCompletion waits for the exec session to complete
func (s *ExecService) waitForCompletion(doneCh <-chan error, errorCh <-chan error, ctx context.Context, stream generated.ExecService_StreamExecServer, execStream types.ExecStream) error {
	var finalErr error
	select {
	case err := <-errorCh:
		finalErr = err
	case <-doneCh:
		// Wait a bit longer for any remaining stdout to be processed
		select {
		case err := <-errorCh:
			finalErr = err
		case <-time.After(500 * time.Millisecond):
		}
	case <-ctx.Done():
	}

	// Get and send exit code
	s.sendExitCode(stream, execStream)

	return finalErr
}

// sendExitCode retrieves and sends the exit code to the client
func (s *ExecService) sendExitCode(stream generated.ExecService_StreamExecServer, execStream types.ExecStream) {
	exitCode := s.getExitCode(execStream)

	// Always send an exit response, even if we can't get the actual exit code
	// This ensures the client knows the session has ended
	if exitCode == -1 {
		// Default to success if we can't determine the actual code
		// Log at debug to avoid alarming users
		s.logger.Debug("Exit code unavailable; defaulting to 0")
		exitCode = 0
	}

	if err := stream.Send(&generated.ExecResponse{
		Response: &generated.ExecResponse_Exit{
			Exit: &generated.ExitInfo{
				Code:     utils.ToInt32NonNegative(exitCode),
				Signaled: exitCode > 128,
			},
		},
	}); err != nil {
		s.logger.Error("Failed to send exit info", log.Err(err))
	}
}

// getExitCode retrieves the exit code with timeout protection
func (s *ExecService) getExitCode(execStream types.ExecStream) int {
	exitCode := -1

	// Try multiple times with increasing delays
	for i := 0; i < 5; i++ {
		select {
		case <-time.After(time.Duration(i+1) * time.Second):
			s.logger.Debug("Attempt to get exit code timed out, retrying", log.Int("attempt", i+1))
		case <-func() chan struct{} {
			ch := make(chan struct{})
			go func() {
				defer close(ch)
				if code, err := execStream.ExitCode(); err == nil {
					exitCode = code
				} else {
					s.logger.Debug("Failed to get exit code, will retry", log.Err(err), log.Int("attempt", i+1))
				}
			}()
			return ch
		}():
			// If we got a valid exit code, return it
			if exitCode != -1 {
				return exitCode
			}
		}
	}

	s.logger.Debug("Failed to get exit code after all attempts")
	return exitCode
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
		strings.Contains(strings.ToLower(msg), "broken pipe") ||
		strings.Contains(strings.ToLower(msg), "connection reset by peer") ||
		strings.Contains(msg, "io: read/write on closed pipe")
}

// validateExecRequest validates an exec request.
func (s *ExecService) validateExecRequest(req *generated.ExecRequest) error {
	// First request must be an init request
	initReq := req.GetInit()
	if initReq == nil {
		return fmt.Errorf("first request must be an init request")
	}

	// Must specify either service name or instance ID
	if initReq.GetServiceName() == "" && initReq.GetInstanceId() == "" {
		return fmt.Errorf("must specify either service_name or instance_id")
	}

	// Can't specify both service name and instance ID
	if initReq.GetServiceName() != "" && initReq.GetInstanceId() != "" {
		return fmt.Errorf("cannot specify both service_name and instance_id")
	}

	// Command must be specified
	if len(initReq.Command) == 0 {
		return fmt.Errorf("command is required")
	}

	return nil
}
