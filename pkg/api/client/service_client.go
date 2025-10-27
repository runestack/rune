package client

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

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

// Helper functions for converting between types.Service and generated.Service
// serviceToProto converts a types.Service to a generated.Service proto message.
func ServiceToProto(service *types.Service) *generated.Service {
	if service == nil {
		return nil
	}

	protoService := &generated.Service{
		Id:        service.ID,
		Name:      service.Name,
		Namespace: service.Namespace,
		Image:     service.Image,
		Command:   service.Command,
		Scale:     utils.ToInt32NonNegative(service.Scale),
		Runtime:   string(service.Runtime),
	}

	if service.Metadata != nil {
		protoService.Metadata = &generated.ServiceMetadata{
			Generation:       utils.ToInt32NonNegative64(service.Metadata.Generation),
			CreatedAt:        service.Metadata.CreatedAt.Format(time.RFC3339),
			UpdatedAt:        service.Metadata.UpdatedAt.Format(time.RFC3339),
			LastNonZeroScale: utils.ToInt32NonNegative(service.Metadata.LastNonZeroScale),
		}
	}

	// Convert args
	if len(service.Args) > 0 {
		protoService.Args = make([]string, len(service.Args))
		copy(protoService.Args, service.Args)
	}

	// Convert environment variables
	if len(service.Env) > 0 {
		protoService.Env = make(map[string]string)
		for k, v := range service.Env {
			protoService.Env[k] = v
		}
	}

	// Convert envFrom sources
	if len(service.EnvFrom) > 0 {
		protoService.EnvFrom = make([]*generated.EnvFromSource, 0, len(service.EnvFrom))
		for _, src := range service.EnvFrom {
			protoService.EnvFrom = append(protoService.EnvFrom, &generated.EnvFromSource{
				SecretName:    src.SecretName,
				ConfigmapName: src.ConfigmapName,
				Namespace:     src.Namespace,
				Prefix:        src.Prefix,
			})
		}
	}

	// Convert ports
	if len(service.Ports) > 0 {
		protoService.Ports = make([]*generated.ServicePort, len(service.Ports))
		for i, port := range service.Ports {
			protoService.Ports[i] = &generated.ServicePort{
				Name:       port.Name,
				Port:       utils.ToInt32NonNegative(port.Port),
				TargetPort: utils.ToInt32NonNegative(port.TargetPort),
				Protocol:   port.Protocol,
			}
		}
	}

	// Convert expose (MVP)
	if service.Expose != nil {
		protoService.Expose = &generated.ServiceExpose{
			Port:     service.Expose.Port,
			Host:     service.Expose.Host,
			HostPort: utils.ToUint32NonNegative(service.Expose.HostPort),
			Path:     service.Expose.Path,
		}
	}

	// Convert resources
	if service.Resources != (types.Resources{}) {
		protoService.Resources = &generated.Resources{
			Cpu: &generated.ResourceLimit{
				Request: service.Resources.CPU.Request,
				Limit:   service.Resources.CPU.Limit,
			},
			Memory: &generated.ResourceLimit{
				Request: service.Resources.Memory.Request,
				Limit:   service.Resources.Memory.Limit,
			},
		}
	}

	// Convert secret mounts
	if len(service.SecretMounts) > 0 {
		protoService.SecretMounts = make([]*generated.SecretMount, len(service.SecretMounts))
		for i, m := range service.SecretMounts {
			items := make([]*generated.KeyToPath, 0, len(m.Items))
			for _, it := range m.Items {
				items = append(items, &generated.KeyToPath{Key: it.Key, Path: it.Path})
			}
			protoService.SecretMounts[i] = &generated.SecretMount{
				Name:       m.Name,
				MountPath:  m.MountPath,
				SecretName: m.SecretName,
				Items:      items,
			}
		}
	}

	// Convert configmap mounts
	if len(service.ConfigmapMounts) > 0 {
		protoService.ConfigmapMounts = make([]*generated.ConfigmapMount, len(service.ConfigmapMounts))
		for i, m := range service.ConfigmapMounts {
			items := make([]*generated.KeyToPath, 0, len(m.Items))
			for _, it := range m.Items {
				items = append(items, &generated.KeyToPath{Key: it.Key, Path: it.Path})
			}
			protoService.ConfigmapMounts[i] = &generated.ConfigmapMount{
				Name:          m.Name,
				MountPath:     m.MountPath,
				ConfigmapName: m.ConfigmapName,
				Items:         items,
			}
		}
	}

	// Convert status
	switch service.Status {
	case types.ServiceStatusPending:
		protoService.Status = generated.ServiceStatus_SERVICE_STATUS_PENDING
	case types.ServiceStatusRunning:
		protoService.Status = generated.ServiceStatus_SERVICE_STATUS_RUNNING
	case types.ServiceStatusDeploying:
		protoService.Status = generated.ServiceStatus_SERVICE_STATUS_UPDATING
	case types.ServiceStatusFailed:
		protoService.Status = generated.ServiceStatus_SERVICE_STATUS_FAILED
	default:
		protoService.Status = generated.ServiceStatus_SERVICE_STATUS_UNSPECIFIED
	}

	// Convert health checks
	if service.Health != nil {
		protoService.Health = &generated.HealthCheck{}

		if service.Health.Liveness != nil {
			protoService.Health.Liveness = &generated.Probe{
				InitialDelaySeconds: utils.ToInt32NonNegative(service.Health.Liveness.InitialDelaySeconds),
				PeriodSeconds:       utils.ToInt32NonNegative(service.Health.Liveness.IntervalSeconds),
				TimeoutSeconds:      utils.ToInt32NonNegative(service.Health.Liveness.TimeoutSeconds),
			}

			switch service.Health.Liveness.Type {
			case "http":
				protoService.Health.Liveness.Type = generated.ProbeType_PROBE_TYPE_HTTP
				protoService.Health.Liveness.Path = service.Health.Liveness.Path
				protoService.Health.Liveness.Port = utils.ToInt32NonNegative(service.Health.Liveness.Port)
			case "tcp":
				protoService.Health.Liveness.Type = generated.ProbeType_PROBE_TYPE_TCP
				protoService.Health.Liveness.Port = utils.ToInt32NonNegative(service.Health.Liveness.Port)
			case "command":
				protoService.Health.Liveness.Type = generated.ProbeType_PROBE_TYPE_COMMAND
				protoService.Health.Liveness.Command = service.Health.Liveness.Command
			}
		}

		if service.Health.Readiness != nil {
			protoService.Health.Readiness = &generated.Probe{
				InitialDelaySeconds: utils.ToInt32NonNegative(service.Health.Readiness.InitialDelaySeconds),
				PeriodSeconds:       utils.ToInt32NonNegative(service.Health.Readiness.IntervalSeconds),
				TimeoutSeconds:      utils.ToInt32NonNegative(service.Health.Readiness.TimeoutSeconds),
			}

			switch service.Health.Readiness.Type {
			case "http":
				protoService.Health.Readiness.Type = generated.ProbeType_PROBE_TYPE_HTTP
				protoService.Health.Readiness.Path = service.Health.Readiness.Path
				protoService.Health.Readiness.Port = utils.ToInt32NonNegative(service.Health.Readiness.Port)
			case "tcp":
				protoService.Health.Readiness.Type = generated.ProbeType_PROBE_TYPE_TCP
				protoService.Health.Readiness.Port = utils.ToInt32NonNegative(service.Health.Readiness.Port)
			case "command":
				protoService.Health.Readiness.Type = generated.ProbeType_PROBE_TYPE_COMMAND
				protoService.Health.Readiness.Command = service.Health.Readiness.Command
			}
		}
	}

	// Dependencies
	if len(service.Dependencies) > 0 {
		protoService.Dependencies = make([]*generated.DependencyRef, 0, len(service.Dependencies))
		for _, d := range service.Dependencies {
			protoService.Dependencies = append(protoService.Dependencies, &generated.DependencyRef{
				Namespace: d.Namespace,
				Service:   d.Service,
				Secret:    d.Secret,
				Configmap: d.Configmap,
			})
		}
	}

	return protoService
}

func ProtoToService(proto *generated.Service) (*types.Service, error) {
	if proto == nil {
		return nil, fmt.Errorf("proto service is nil")
	}

	// Create an initial service with basic fields
	service := &types.Service{
		ID:        proto.Id,
		Name:      proto.Name,
		Namespace: proto.Namespace,
		Image:     proto.Image,
		Command:   proto.Command,
		Scale:     int(proto.Scale),
		Runtime:   types.RuntimeType(proto.Runtime),
	}

	// Convert metadata
	if proto.Metadata != nil {
		if service.Metadata == nil {
			service.Metadata = &types.ServiceMetadata{}
		}
		service.Metadata.Generation = int64(proto.Metadata.Generation)
		service.Metadata.LastNonZeroScale = int(proto.Metadata.LastNonZeroScale)

		createdAt, err := utils.ParseTimestamp(proto.Metadata.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to parse created_at timestamp: %w", err)
		}
		service.Metadata.CreatedAt = *createdAt

		updatedAt, err := utils.ParseTimestamp(proto.Metadata.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to parse updated_at timestamp: %w", err)
		}
		service.Metadata.UpdatedAt = *updatedAt
	}

	// Convert args
	if len(proto.Args) > 0 {
		service.Args = make([]string, len(proto.Args))
		copy(service.Args, proto.Args)
	}

	// Convert environment variables
	if len(proto.Env) > 0 {
		service.Env = make(map[string]string)
		for k, v := range proto.Env {
			service.Env[k] = v
		}
	}

	// Convert envFrom sources
	if len(proto.EnvFrom) > 0 {
		service.EnvFrom = make([]types.EnvFromSource, 0, len(proto.EnvFrom))
		for _, src := range proto.EnvFrom {
			service.EnvFrom = append(service.EnvFrom, types.EnvFromSource{
				SecretName:    src.SecretName,
				ConfigmapName: src.ConfigmapName,
				Namespace:     src.Namespace,
				Prefix:        src.Prefix,
			})
		}
	}

	// Convert ports
	if len(proto.Ports) > 0 {
		service.Ports = make([]types.ServicePort, len(proto.Ports))
		for i, port := range proto.Ports {
			service.Ports[i] = types.ServicePort{
				Name:       port.Name,
				Port:       int(port.Port),
				TargetPort: int(port.TargetPort),
				Protocol:   port.Protocol,
			}
		}
	}

	// Convert expose (MVP)
	if proto.Expose != nil {
		service.Expose = &types.ServiceExpose{
			Port:     proto.Expose.Port,
			Host:     proto.Expose.Host,
			HostPort: int(proto.Expose.HostPort),
			Path:     proto.Expose.Path,
		}
	}

	// Convert resources
	if proto.Resources != nil {
		if proto.Resources.Cpu != nil {
			service.Resources.CPU = types.ResourceLimit{
				Request: proto.Resources.Cpu.Request,
				Limit:   proto.Resources.Cpu.Limit,
			}
		}
		if proto.Resources.Memory != nil {
			service.Resources.Memory = types.ResourceLimit{
				Request: proto.Resources.Memory.Request,
				Limit:   proto.Resources.Memory.Limit,
			}
		}
	}

	// Convert secret mounts
	if len(proto.SecretMounts) > 0 {
		service.SecretMounts = make([]types.SecretMount, len(proto.SecretMounts))
		for i, m := range proto.SecretMounts {
			items := make([]types.KeyToPath, 0, len(m.Items))
			for _, it := range m.Items {
				items = append(items, types.KeyToPath{Key: it.Key, Path: it.Path})
			}
			service.SecretMounts[i] = types.SecretMount{
				Name:       m.Name,
				MountPath:  m.MountPath,
				SecretName: m.SecretName,
				Items:      items,
			}
		}
	}

	// Convert configmap mounts
	if len(proto.ConfigmapMounts) > 0 {
		service.ConfigmapMounts = make([]types.ConfigmapMount, len(proto.ConfigmapMounts))
		for i, m := range proto.ConfigmapMounts {
			items := make([]types.KeyToPath, 0, len(m.Items))
			for _, it := range m.Items {
				items = append(items, types.KeyToPath{Key: it.Key, Path: it.Path})
			}
			service.ConfigmapMounts[i] = types.ConfigmapMount{
				Name:          m.Name,
				MountPath:     m.MountPath,
				ConfigmapName: m.ConfigmapName,
				Items:         items,
			}
		}
	}

	// Convert status
	switch proto.Status {
	case generated.ServiceStatus_SERVICE_STATUS_PENDING:
		service.Status = types.ServiceStatusPending
	case generated.ServiceStatus_SERVICE_STATUS_RUNNING:
		service.Status = types.ServiceStatusRunning
	case generated.ServiceStatus_SERVICE_STATUS_UPDATING:
		service.Status = types.ServiceStatusDeploying
	case generated.ServiceStatus_SERVICE_STATUS_FAILED:
		service.Status = types.ServiceStatusFailed
	default:
		service.Status = types.ServiceStatusPending
	}

	// Convert health check
	if proto.Health != nil {
		service.Health = &types.HealthCheck{}

		if proto.Health.Liveness != nil {
			service.Health.Liveness = &types.Probe{
				InitialDelaySeconds: int(proto.Health.Liveness.InitialDelaySeconds),
				IntervalSeconds:     int(proto.Health.Liveness.PeriodSeconds),
				TimeoutSeconds:      int(proto.Health.Liveness.TimeoutSeconds),
			}

			switch proto.Health.Liveness.Type {
			case generated.ProbeType_PROBE_TYPE_HTTP:
				service.Health.Liveness.Type = "http"
				service.Health.Liveness.Path = proto.Health.Liveness.Path
				service.Health.Liveness.Port = int(proto.Health.Liveness.Port)
			case generated.ProbeType_PROBE_TYPE_TCP:
				service.Health.Liveness.Type = "tcp"
				service.Health.Liveness.Port = int(proto.Health.Liveness.Port)
			case generated.ProbeType_PROBE_TYPE_COMMAND:
				service.Health.Liveness.Type = "command"
				service.Health.Liveness.Command = proto.Health.Liveness.Command
			}
		}

		if proto.Health.Readiness != nil {
			service.Health.Readiness = &types.Probe{
				InitialDelaySeconds: int(proto.Health.Readiness.InitialDelaySeconds),
				IntervalSeconds:     int(proto.Health.Readiness.PeriodSeconds),
				TimeoutSeconds:      int(proto.Health.Readiness.TimeoutSeconds),
			}

			switch proto.Health.Readiness.Type {
			case generated.ProbeType_PROBE_TYPE_HTTP:
				service.Health.Readiness.Type = "http"
				service.Health.Readiness.Path = proto.Health.Readiness.Path
				service.Health.Readiness.Port = int(proto.Health.Readiness.Port)
			case generated.ProbeType_PROBE_TYPE_TCP:
				service.Health.Readiness.Type = "tcp"
				service.Health.Readiness.Port = int(proto.Health.Readiness.Port)
			case generated.ProbeType_PROBE_TYPE_COMMAND:
				service.Health.Readiness.Type = "command"
				service.Health.Readiness.Command = proto.Health.Readiness.Command
			}
		}
	}

	// Dependencies
	if len(proto.Dependencies) > 0 {
		service.Dependencies = make([]types.DependencyRef, 0, len(proto.Dependencies))
		for _, d := range proto.Dependencies {
			service.Dependencies = append(service.Dependencies, types.DependencyRef{
				Service:   d.Service,
				Namespace: d.Namespace,
				Secret:    d.Secret,
				Configmap: d.Configmap,
			})
		}
	}

	return service, nil
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
