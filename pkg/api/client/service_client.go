package client

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ServiceClient provides methods for interacting with services on the Rune API server.
type ServiceClient struct {
	client *Client
	logger log.Logger
	svc    generated.ServiceServiceClient
}

// NewServiceClient creates a new service client.
func NewServiceClient(client *Client) *ServiceClient {
	return &ServiceClient{
		client: client,
		logger: client.logger.WithComponent("service-client"),
		svc:    generated.NewServiceServiceClient(client.conn),
	}
}

// GetLogger returns the logger for this client
func (s *ServiceClient) GetLogger() log.Logger {
	return s.logger
}

// CreateService creates a new service on the API server.
func (s *ServiceClient) CreateService(service *types.Service, ensureNamespace bool) error {
	s.logger.Debug("Creating service", log.Str("name", service.Name), log.Str("namespace", service.Namespace))

	// Create the gRPC request
	req := &generated.CreateServiceRequest{
		Service:         ServiceToProto(service),
		EnsureNamespace: ensureNamespace,
	}

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.CreateService(ctx, req)
	if err != nil {
		s.logger.Error("Failed to create service", log.Err(err), log.Str("name", service.Name))
		return convertGRPCError("create service", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		s.logger.Error("Failed to create service", log.Err(err), log.Str("name", service.Name))
		return err
	}

	return nil
}

// GetService retrieves a service by name.
func (s *ServiceClient) GetService(namespace, name string) (*types.Service, error) {
	s.logger.Debug("Getting service", log.Str("name", name), log.Str("namespace", namespace))

	// Create the gRPC request
	req := &generated.GetServiceRequest{
		Name:      name,
		Namespace: namespace,
	}

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.GetService(ctx, req)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return nil, fmt.Errorf("service not found: %s/%s", namespace, name)
		}
		s.logger.Error("Failed to get service", log.Err(err), log.Str("name", name))
		return nil, convertGRPCError("get service", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		s.logger.Error("Failed to get service", log.Err(err), log.Str("name", name))
		return nil, err
	}

	// Convert the proto message to a service
	service, err := ProtoToService(resp.Service)
	if err != nil {
		return nil, fmt.Errorf("failed to convert service: %w", err)
	}

	return service, nil
}

// UpdateService updates an existing service.
func (s *ServiceClient) UpdateService(service *types.Service, force bool) error {
	s.logger.Debug("Updating service",
		log.Str("name", service.Name),
		log.Str("namespace", service.Namespace),
		log.Bool("force", force))

	// Create the gRPC request
	req := &generated.UpdateServiceRequest{
		Service: ServiceToProto(service),
		Force:   force,
	}

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.UpdateService(ctx, req)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return fmt.Errorf("service not found: %s/%s", service.Namespace, service.Name)
		}
		s.logger.Error("Failed to update service", log.Err(err), log.Str("name", service.Name))
		return convertGRPCError("update service", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		s.logger.Error("Failed to update service", log.Err(err), log.Str("name", service.Name))
		return err
	}

	return nil
}

// DeleteService deletes a service by name.
func (s *ServiceClient) DeleteService(namespace, name string) error {
	s.logger.Debug("Deleting service", log.Str("name", name), log.Str("namespace", namespace))

	// Create the gRPC request
	req := &generated.DeleteServiceRequest{
		Name:      name,
		Namespace: namespace,
	}

	// Use the enhanced delete method
	_, err := s.DeleteServiceWithRequest(req)
	return err
}

// DeleteServiceWithRequest deletes a service with the full request object.
func (s *ServiceClient) DeleteServiceWithRequest(req *generated.DeleteServiceRequest) (*generated.DeleteServiceResponse, error) {
	s.logger.Debug("Deleting service with options",
		log.Str("name", req.Name),
		log.Str("namespace", req.Namespace),
		log.Bool("force", req.Force),
		log.Bool("dry_run", req.DryRun),
		log.Bool("detach", req.Detach))

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.DeleteService(ctx, req)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return nil, fmt.Errorf("service not found: %s/%s", req.Namespace, req.Name)
		}
		s.logger.Error("Failed to delete service", log.Err(err), log.Str("name", req.Name))
		return nil, convertGRPCError("delete service", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		s.logger.Error("Failed to delete service", log.Err(err), log.Str("name", req.Name))
		return nil, err
	}

	return resp, nil
}

// GetDeletionStatus gets the status of a deletion operation.
func (s *ServiceClient) GetDeletionStatus(namespace, name string) (*generated.GetDeletionStatusResponse, error) {
	s.logger.Debug("Getting deletion status", log.Str("namespace", namespace), log.Str("name", name))

	// Create the gRPC request
	req := &generated.GetDeletionStatusRequest{
		Namespace: namespace,
		Name:      name,
	}

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.GetDeletionStatus(ctx, req)
	if err != nil {
		s.logger.Error("Failed to get deletion status", log.Err(err), log.Str("deletion_id", name))
		return nil, convertGRPCError("get deletion status", err)
	}

	return resp, nil
}

// ListDeletionOperations lists all deletion operations.
func (s *ServiceClient) ListDeletionOperations(namespace, status string) (*generated.ListDeletionOperationsResponse, error) {
	s.logger.Debug("Listing deletion operations", log.Str("namespace", namespace), log.Str("status", status))

	// Create the gRPC request
	req := &generated.ListDeletionOperationsRequest{
		Namespace: namespace,
		Status:    status,
	}

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.ListDeletionOperations(ctx, req)
	if err != nil {
		s.logger.Error("Failed to list deletion operations", log.Err(err))
		return nil, convertGRPCError("list deletion operations", err)
	}

	return resp, nil
}

// ListServices lists services in a namespace with optional filtering.
func (s *ServiceClient) ListServices(namespace string, labelSelector string, fieldSelector string) ([]*types.Service, error) {
	s.logger.Debug("Listing services",
		log.Str("namespace", namespace),
		log.Str("labelSelector", labelSelector),
		log.Str("fieldSelector", fieldSelector))

	// Create the gRPC request
	req := &generated.ListServicesRequest{
		Namespace:     namespace,
		LabelSelector: make(map[string]string),
		FieldSelector: make(map[string]string),
	}

	// Parse label selector if provided
	if labelSelector != "" {
		labels, err := parseSelector(labelSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid label selector: %w", err)
		}
		req.LabelSelector = labels
	}

	// Parse field selector if provided
	if fieldSelector != "" {
		fields, err := parseSelector(fieldSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid field selector: %w", err)
		}
		req.FieldSelector = fields
	}

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.ListServices(ctx, req)
	if err != nil {
		s.logger.Error("Failed to list services", log.Err(err), log.Str("namespace", namespace))
		return nil, convertGRPCError("list services", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		s.logger.Error("Failed to list services", log.Err(err), log.Str("namespace", namespace))
		return nil, err
	}

	// Convert the proto messages to services
	services := make([]*types.Service, 0, len(resp.Services))
	for _, protoService := range resp.Services {
		service, err := ProtoToService(protoService)
		if err != nil {
			s.logger.Error("Failed to convert service", log.Err(err))
			continue
		}
		services = append(services, service)
	}

	return services, nil
}

// Helper function for parsing key=value selectors
func parseSelector(selector string) (map[string]string, error) {
	result := make(map[string]string)
	if selector == "" {
		return result, nil
	}

	pairs := strings.Split(selector, ",")
	for _, pair := range pairs {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid selector format, expected key=value: %s", pair)
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" {
			return nil, fmt.Errorf("empty key in selector: %s", pair)
		}
		if value == "" {
			return nil, fmt.Errorf("empty value in selector: %s", pair)
		}
		result[key] = value
	}

	return result, nil
}

// ScaleService changes the scale of a service.
func (s *ServiceClient) ScaleService(namespace, name string, scale int) error {
	s.logger.Debug("Scaling service", log.Str("name", name), log.Str("namespace", namespace), log.Int("scale", scale))

	// Create the gRPC request
	req := &generated.ScaleServiceRequest{
		Name:      name,
		Namespace: namespace,
		Scale:     utils.ToInt32NonNegative(scale),
	}

	// Send the request to the API server
	_, err := s.ScaleServiceWithRequest(req)
	return err
}

// RestartService restarts a service in place (issue #140): the server stamps
// a new template generation and the reconciler replaces every instance at the
// current spec — the desired scale never dips through zero. Returns the
// stamped template generation and the scale the service converges to; callers
// wait for all instances to be Running with generation >= the returned value.
func (s *ServiceClient) RestartService(namespace, name string) (templateGeneration int64, scale int, err error) {
	s.logger.Debug("Restarting service", log.Str("name", name), log.Str("namespace", namespace))

	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.RestartService(ctx, &generated.RestartServiceRequest{
		Name:      name,
		Namespace: namespace,
	})
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return 0, 0, fmt.Errorf("service not found: %s/%s", namespace, name)
		}
		s.logger.Error("Failed to restart service", log.Err(err), log.Str("name", name))
		return 0, 0, convertGRPCError("restart service", err)
	}

	return resp.TemplateGeneration, int(resp.Scale), nil
}

// ScaleServiceWithRequest changes the scale of a service with the full request object.
func (s *ServiceClient) ScaleServiceWithRequest(req *generated.ScaleServiceRequest) (*generated.ServiceResponse, error) {
	s.logger.Debug("Scaling service with options",
		log.Str("name", req.Name),
		log.Str("namespace", req.Namespace),
		log.Int("scale", int(req.Scale)),
	)

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.ScaleService(ctx, req)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return nil, fmt.Errorf("service not found: %s/%s", req.Namespace, req.Name)
		}
		s.logger.Error("Failed to scale service", log.Err(err), log.Str("name", req.Name))
		return nil, convertGRPCError("scale service", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		s.logger.Error("Failed to scale service", log.Err(err), log.Str("name", req.Name))
		return nil, err
	}

	return resp, nil
}

// convertGRPCError converts a gRPC error to a more user-friendly error message.
func convertGRPCError(operation string, err error) error {
	statusErr, ok := status.FromError(err)
	if !ok {
		// Not a gRPC error
		return fmt.Errorf("failed to %s: %w", operation, err)
	}

	switch statusErr.Code() {
	case codes.NotFound:
		return fmt.Errorf("resource not found: %s", statusErr.Message())
	case codes.AlreadyExists:
		return fmt.Errorf("resource already exists: %s", statusErr.Message())
	case codes.InvalidArgument:
		return fmt.Errorf("invalid argument: %s", statusErr.Message())
	case codes.FailedPrecondition:
		return fmt.Errorf("failed precondition: %s", statusErr.Message())
	case codes.PermissionDenied:
		return fmt.Errorf("permission denied: %s", statusErr.Message())
	case codes.Unauthenticated:
		return fmt.Errorf("unauthenticated: %s", statusErr.Message())
	case codes.ResourceExhausted:
		return fmt.Errorf("resource exhausted: %s", statusErr.Message())
	case codes.Unavailable:
		return fmt.Errorf("service unavailable: %s", statusErr.Message())
	default:
		return fmt.Errorf("failed to %s: %s (code %d)", operation, statusErr.Message(), statusErr.Code())
	}
}

// WatchEvent represents a service change event
type WatchEvent struct {
	Service   *types.Service
	EventType string // "ADDED", "MODIFIED", "DELETED"
	Error     error
}

// WatchServices watches services for changes and returns a channel of events.
// The caller should call the cancel function when done watching to prevent resource leaks.
func (s *ServiceClient) WatchServices(namespace string, labelSelector string, fieldSelector string) (<-chan WatchEvent, context.CancelFunc, error) {
	s.logger.Debug("Watching services",
		log.Str("namespace", namespace),
		log.Str("labelSelector", labelSelector),
		log.Str("fieldSelector", fieldSelector))

	// Create the gRPC request
	req := &generated.WatchServicesRequest{
		Namespace:     namespace,
		LabelSelector: make(map[string]string),
		FieldSelector: make(map[string]string),
	}

	// Parse label selector if provided
	if labelSelector != "" {
		labels, err := parseSelector(labelSelector)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid label selector: %w", err)
		}
		req.LabelSelector = labels
	}

	// Parse field selector if provided
	if fieldSelector != "" {
		fields, err := parseSelector(fieldSelector)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid field selector: %w", err)
		}
		req.FieldSelector = fields
	}

	// Create context with cancel
	ctx, cancel := context.WithCancel(context.Background())

	// Establish the streaming connection
	stream, err := s.svc.WatchServices(ctx, req)
	if err != nil {
		cancel()
		s.logger.Error("Failed to establish watch connection", log.Err(err))
		return nil, nil, convertGRPCError("watch services", err)
	}

	// Create channel for watch events
	eventCh := make(chan WatchEvent)

	// Start goroutine to receive watch events and send them to the channel
	go func() {
		defer close(eventCh)

		for {
			// Check if context is cancelled
			select {
			case <-ctx.Done():
				s.logger.Debug("Watch context cancelled")
				return
			default:
				// Continue processing
			}

			// Receive event from server
			resp, err := stream.Recv()
			if err == io.EOF {
				s.logger.Debug("Watch stream closed by server")
				return
			}
			if err != nil {
				// Check if error is due to context cancellation (expected behavior)
				if ctx.Err() != nil {
					s.logger.Debug("Watch cancelled", log.Err(err))
					return
				}
				s.logger.Error("Error receiving watch event", log.Err(err))
				eventCh <- WatchEvent{
					Error: fmt.Errorf("watch error: %w", err),
				}
				return
			}

			// Check if the API returned an error status
			if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
				err := fmt.Errorf("API error: %s", resp.Status.Message)
				s.logger.Error("Watch API error", log.Err(err))
				eventCh <- WatchEvent{
					Error: err,
				}
				return
			}

			// Convert proto event type to string
			var eventType string
			switch resp.EventType {
			case generated.EventType_EVENT_TYPE_ADDED:
				eventType = "ADDED"
			case generated.EventType_EVENT_TYPE_MODIFIED:
				eventType = "MODIFIED"
			case generated.EventType_EVENT_TYPE_DELETED:
				eventType = "DELETED"
			default:
				eventType = "UNKNOWN"
			}

			// Convert the proto service to a type service
			service, err := ProtoToService(resp.Service)
			if err != nil {
				s.logger.Error("Failed to convert service", log.Err(err))
				eventCh <- WatchEvent{
					Error: fmt.Errorf("failed to convert service: %w", err),
				}
				continue
			}

			// Send the event to the channel
			eventCh <- WatchEvent{
				Service:   service,
				EventType: eventType,
				Error:     nil,
			}
		}
	}()

	return eventCh, cancel, nil
}

// ListInstances lists instances for a service.
func (s *ServiceClient) ListInstances(req *generated.ListInstancesRequest) (*generated.ListInstancesResponse, error) {
	s.logger.Debug("Listing instances",
		log.Str("service", req.ServiceName),
		log.Str("namespace", req.Namespace))

	// Send the request to the API server
	ctx, cancel := s.client.Context()
	defer cancel()

	resp, err := s.svc.ListInstances(ctx, req)
	if err != nil {
		s.logger.Error("Failed to list instances", log.Err(err))
		return nil, convertGRPCError("list instances", err)
	}

	return resp, nil
}

// WatchScaling observes the scaling progress of a service and returns a channel of status updates.
// The caller should close the cancel function when done watching to prevent resource leaks.
func (s *ServiceClient) WatchScaling(namespace, name string, targetScale int) (<-chan *generated.ScalingStatusResponse, context.CancelFunc, error) {
	s.logger.Debug("Watching scaling for service",
		log.Str("name", name),
		log.Str("namespace", namespace),
		log.Int("targetScale", targetScale))

	// Create a request for the API server
	req := &generated.WatchScalingRequest{
		ServiceName: name,
		Namespace:   namespace,
		TargetScale: utils.ToInt32NonNegative(targetScale),
	}

	// Create a context with cancel
	ctx, cancel := context.WithCancel(context.Background())

	// Create a channel to send events to the caller
	eventCh := make(chan *generated.ScalingStatusResponse, 10)

	// Call the API in a separate goroutine
	go func() {
		defer close(eventCh)

		stream, err := s.svc.WatchScaling(ctx, req)
		if err != nil {
			statusErr, ok := status.FromError(err)
			errMsg := err.Error()
			if ok {
				errMsg = statusErr.Message()
			}
			s.logger.Error("Failed to watch scaling", log.Err(err), log.Str("name", name))
			// Send an error status
			eventCh <- &generated.ScalingStatusResponse{
				Status: &generated.Status{
					Code:    int32(codes.Internal),
					Message: fmt.Sprintf("Failed to watch scaling: %s", errMsg),
				},
			}
			return
		}

		// Continuously receive messages until the context is canceled or the stream ends
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				// Stream ended normally
				return
			}
			if err != nil {
				// Check if context was canceled
				if ctx.Err() != nil {
					return
				}
				s.logger.Error("Error receiving scaling status", log.Err(err), log.Str("name", name))
				// Send error to channel
				eventCh <- &generated.ScalingStatusResponse{
					Status: &generated.Status{
						Code:    int32(codes.Internal),
						Message: fmt.Sprintf("Stream error: %s", err.Error()),
					},
				}
				return
			}

			// Send the event to the caller
			select {
			case eventCh <- resp:
				// Sent successfully
			case <-ctx.Done():
				// Context was canceled, exit the goroutine
				return
			}
		}
	}()

	return eventCh, cancel, nil
}
