package service

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// LogService implements the gRPC LogService.
type LogService struct {
	generated.UnimplementedLogServiceServer

	store        store.Store
	logger       log.Logger
	orchestrator orchestrator.Orchestrator
}

// NewLogService creates a new LogService with the given runners, store, and logger.
func NewLogService(store store.Store, logger log.Logger, orchestrator orchestrator.Orchestrator) *LogService {
	return &LogService{
		store:        store,
		logger:       logger.WithComponent("log-service"),
		orchestrator: orchestrator,
	}
}

// parseLogLine parses a log line from MultiLogStreamer and converts it to a LogResponse
// Format from MultiLogStreamer is: @@LOG_META|[instanceID|instanceName|timestamp]@@ content
func (s *LogService) parseLogLine(line, serviceName, fallbackInstanceName string) *generated.LogResponse {
	// Extract metadata using the orchestrator's function
	instanceID, instanceName, timestamp, content := utils.ExtractLineMetadata(line)

	// Use fallback instance ID if not found in metadata
	if instanceName == "" {
		instanceName = fallbackInstanceName
	}

	// Determine log level from content
	logLevel := "info" // Default level
	if strings.Contains(strings.ToLower(content), "error") ||
		strings.Contains(strings.ToLower(content), "exception") ||
		strings.Contains(strings.ToLower(content), "failed") {
		logLevel = "error"
	} else if strings.Contains(strings.ToLower(content), "warn") {
		logLevel = "warning"
	}

	// Create and return the LogResponse
	return &generated.LogResponse{
		ServiceName:  serviceName,
		InstanceId:   instanceID,
		InstanceName: instanceName,
		Timestamp:    timestamp,
		Content:      content,
		Stream:       "stdout",
		LogLevel:     logLevel,
	}
}

// readLogsFromReader scans the combined log reader and emits parsed responses
// on logCh. It owns logCh and closes it on completion (EOF, context cancel, or
// scanner error) so downstream consumers can detect end-of-stream.
func (s *LogService) readLogsFromReader(ctx context.Context, logReader io.ReadCloser, logCh chan<- *generated.LogResponse, errCh chan<- error, serviceName, instanceName string) {
	defer close(logCh)
	defer func() {
		if r := recover(); r != nil {
			s.logger.Debug("Recovered from panic in readLogsFromReader", log.Any("recover", r))
		}
	}()

	scanner := bufio.NewScanner(logReader)
	// Allow long log lines (default bufio.Scanner caps at 64KB).
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		select {
		case <-ctx.Done():
			s.logger.Debug("Context cancelled, stopping log reading")
			return
		case logCh <- s.parseLogLine(line, serviceName, instanceName):
		}
	}

	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			s.logger.Debug("Scanner error after context cancellation", log.Err(err))
			return
		}
		s.logger.Error("Failed to scan logs", log.Err(err))
		select {
		case <-ctx.Done():
		case errCh <- fmt.Errorf("failed to scan logs: %v", err):
		}
		return
	}

	s.logger.Debug("End of logs")
}

// buildLogOptions converts a log request into log options
func (s *LogService) buildLogOptions(req *generated.LogRequest) (types.LogOptions, error) {
	logOptions := types.LogOptions{
		Follow:     req.Follow,
		Tail:       int(req.Tail),
		Timestamps: req.Timestamps,
		ShowLogs:   req.ShowLogs,
		ShowEvents: req.ShowEvents,
		ShowStatus: req.ShowStatus,
	}

	// If none of the show options are specified, default to showing all
	if !req.ShowLogs && !req.ShowEvents && !req.ShowStatus {
		logOptions.ShowLogs = true
		logOptions.ShowEvents = true
		logOptions.ShowStatus = true
	}

	// Parse timestamps if provided
	if req.Since != "" {
		since, err := time.Parse(time.RFC3339, req.Since)
		if err != nil {
			return logOptions, fmt.Errorf("invalid since timestamp: %v", err)
		}
		logOptions.Since = since
	}

	if req.Until != "" {
		until, err := time.Parse(time.RFC3339, req.Until)
		if err != nil {
			return logOptions, fmt.Errorf("invalid until timestamp: %v", err)
		}
		logOptions.Until = until
	}

	return logOptions, nil
}

// getLogReader returns the appropriate log reader based on the request type
func (s *LogService) getLogReader(ctx context.Context, req *generated.LogRequest, logOptions types.LogOptions) (io.ReadCloser, string, string, error) {
	var logReader io.ReadCloser
	var err error
	var serviceName string
	var instanceName string

	resourceTarget, err := resolveResourceTarget(ctx, s.store, req.ResourceTarget, types.NS(req.Namespace))
	if err != nil {
		s.logger.Error("Failed to resolve resource target", log.Err(err))
		return nil, "", "", status.Errorf(codes.InvalidArgument, "invalid resource target: %v", err)
	}

	switch resourceTarget.Type {
	case types.ResourceTypeService:
		service, err := resourceTarget.GetService()
		if err != nil {
			return nil, "", "", status.Errorf(codes.Internal, "resource is not a service: %v", err)
		}
		serviceName = service.Name

		logReader, err = s.orchestrator.GetServiceLogs(ctx, types.NS(req.Namespace), service.Name, logOptions)
		if err != nil {
			s.logger.Error("Failed to get service logs",
				log.Str("service", serviceName),
				log.Err(err))
			return nil, "", "", status.Errorf(codes.Internal, "failed to get service logs: %v", err)
		}

	case types.ResourceTypeInstance:
		instance, err := resourceTarget.GetInstance()
		if err != nil {
			return nil, "", "", status.Errorf(codes.Internal, "resource is not an instance: %v", err)
		}
		instanceName = instance.Name
		// Fetch the parent service name for richer prefixes/output.
		if instance.ServiceName != "" {
			serviceName = instance.ServiceName
		}

		logReader, err = s.orchestrator.GetInstanceLogs(ctx, types.NS(req.Namespace), instance.ID, logOptions)
		if err != nil {
			s.logger.Error("Failed to get instance logs",
				log.Str("instance", instanceName),
				log.Err(err))
			return nil, "", "", status.Errorf(codes.Internal, "failed to get instance logs: %v", err)
		}

	default:
		return nil, "", "", status.Errorf(codes.InvalidArgument, "unsupported resource type: %s", resourceTarget.Type)
	}

	if logReader == nil {
		return nil, "", "", status.Errorf(codes.Internal, "failed to create log reader")
	}

	return logReader, serviceName, instanceName, nil
}

// watchClientStream consumes any follow-up messages from the client so we
// notice when the client closes the stream. Parameter-update support is not
// implemented yet; for now we just drain and watch for EOF/cancel so we can
// shut down cleanly. Cancels the supplied context on EOF or recv error.
func (s *LogService) watchClientStream(ctx context.Context, cancel context.CancelFunc, stream generated.LogService_StreamLogsServer) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Debug("Recovered from panic in watchClientStream", log.Any("recover", r))
		}
	}()

	for {
		if _, err := stream.Recv(); err != nil {
			if err != io.EOF && ctx.Err() == nil {
				s.logger.Debug("Client stream receive ended", log.Err(err))
			}
			cancel()
			return
		}
		// Future: handle parameter updates here.
		if ctx.Err() != nil {
			return
		}
	}
}

// StreamLogs provides bidirectional streaming for logs.
func (s *LogService) StreamLogs(stream generated.LogService_StreamLogsServer) error {
	// Get the initial request
	req, err := stream.Recv()
	if err != nil {
		s.logger.Error("Failed to receive initial log request", log.Err(err))
		return status.Errorf(codes.Internal, "failed to receive initial request: %v", err)
	}

	// Validate the request
	if err := s.validateLogRequest(req); err != nil {
		s.logger.Error("Invalid log request", log.Err(err))
		return status.Errorf(codes.InvalidArgument, "invalid log request: %v", err)
	}

	// Set up context with cancel
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	// Build log options from request
	logOptions, err := s.buildLogOptions(req)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	// Get the appropriate log reader
	logReader, serviceName, instanceName, err := s.getLogReader(ctx, req, logOptions)
	if err != nil {
		return err
	}
	defer logReader.Close()

	// Channel to collect log output. The reader goroutine owns it and closes
	// it on EOF/cancel so the sender can detect end-of-stream.
	logCh := make(chan *generated.LogResponse, 100)

	// Error channel to propagate errors from goroutines
	errCh := make(chan error, 1)

	// Start a goroutine to read from logReader and send to logCh
	go s.readLogsFromReader(ctx, logReader, logCh, errCh, serviceName, instanceName)

	// Watch the client stream so we shut down when the client cancels.
	go s.watchClientStream(ctx, cancel, stream)

	// Stream logs to client
	return s.streamLogsToClient(ctx, stream, logCh, errCh)
}

// streamLogsToClient sends log responses to the client. It returns nil when
// the reader signals end-of-stream by closing logCh (e.g. non-follow logs were
// fully consumed) so the gRPC stream is half-closed and the client sees io.EOF.
func (s *LogService) streamLogsToClient(ctx context.Context, stream generated.LogService_StreamLogsServer,
	logCh <-chan *generated.LogResponse, errCh <-chan error) error {

	for {
		select {
		case logResp, ok := <-logCh:
			if !ok {
				return nil
			}
			if err := stream.Send(logResp); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				s.logger.Error("Failed to send log response", log.Err(err))
				return status.Errorf(codes.Internal, "failed to send log response: %v", err)
			}
		case err := <-errCh:
			if err == nil {
				continue
			}
			s.logger.Error("Log streaming error", log.Err(err))
			return status.Errorf(codes.Internal, "log streaming error: %v", err)
		case <-ctx.Done():
			s.logger.Debug("Context cancelled")
			return nil
		}
	}
}

// validateLogRequest validates a log request.
func (s *LogService) validateLogRequest(req *generated.LogRequest) error {
	if req.ResourceTarget == "" {
		return fmt.Errorf("must specify either service name or instance ID or resource type/name")
	}

	// Validate since and until timestamps
	if req.Since != "" {
		if _, err := time.Parse(time.RFC3339, req.Since); err != nil {
			return fmt.Errorf("invalid since timestamp: %v", err)
		}
	}

	if req.Until != "" {
		if _, err := time.Parse(time.RFC3339, req.Until); err != nil {
			return fmt.Errorf("invalid until timestamp: %v", err)
		}
	}

	return nil
}
