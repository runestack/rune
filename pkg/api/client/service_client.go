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

// Helper functions for converting between types.Service and generated.Service
// serviceToProto converts a types.Service to a generated.Service proto message.
func ServiceToProto(service *types.Service) *generated.Service {
	if service == nil {
		return nil
	}

	protoService := &generated.Service{
		Id:                 service.ID,
		Name:               service.Name,
		Namespace:          service.Namespace,
		Image:              service.Image,
		Command:            service.Command,
		Scale:              utils.ToInt32NonNegative(service.Scale),
		Runtime:            string(service.Runtime),
		ImagePull:          service.ImagePull,
		ImagePullAnonymous: service.ImagePullAnonymous,
		StatusReason:       service.StatusReason,
		StatusMessage:      service.StatusMessage,
		Labels:             service.Labels,
	}

	if service.Metadata != nil {
		protoService.Metadata = &generated.ServiceMetadata{
			Generation:         service.Metadata.Generation,
			CreatedAt:          service.Metadata.CreatedAt.Format(time.RFC3339),
			UpdatedAt:          service.Metadata.UpdatedAt.Format(time.RFC3339),
			LastNonZeroScale:   utils.ToInt32NonNegative(service.Metadata.LastNonZeroScale),
			TemplateGeneration: service.Metadata.TemplateGeneration,
			ObservedGeneration: service.Metadata.ObservedGeneration,
		}
	}

	// Update strategy + drain grace (RUNE-042). Both are optional on the
	// wire: an unset strategy means rolling, and drain_seconds 0 means "not
	// specified" so the server applies the default.
	if service.UpdateStrategy != nil {
		protoService.UpdateStrategy = &generated.UpdateStrategy{
			Type: string(service.UpdateStrategy.Type),
		}
	}
	if service.DrainSeconds != nil {
		protoService.DrainSeconds = utils.ToInt32NonNegative(*service.DrainSeconds)
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
				HostPort:   utils.ToInt32NonNegative(port.HostPort),
			}
		}
	}

	// Convert expose
	if service.Expose != nil {
		protoService.Expose = &generated.ServiceExpose{
			Port:       service.Expose.Port,
			Host:       service.Expose.Host,
			Path:       service.Expose.Path,
			AllowCidrs: service.Expose.AllowCIDRs,
		}
		if service.Expose.TLS != nil {
			protoService.Expose.Tls = &generated.ExposeServiceTLS{
				Secret: service.Expose.TLS.Secret,
				Auto:   service.Expose.TLS.Auto,
				Mode:   service.Expose.TLS.Mode,
			}
		}
		if service.Expose.ClientCert != nil {
			protoService.Expose.ClientCert = &generated.ExposeClientCert{
				CaSecret: service.Expose.ClientCert.CASecret,
				Mode:     service.Expose.ClientCert.Mode,
			}
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

	// Convert volume mounts (RUNE-070/072). Exactly one of claim /
	// claim_template is set per mount; binding state lives on the Volume
	// resource itself and is not transported on the Service.
	if len(service.Volumes) > 0 {
		protoService.Volumes = make([]*generated.VolumeMount, len(service.Volumes))
		for i, m := range service.Volumes {
			pv := &generated.VolumeMount{
				Name:      m.Name,
				MountPath: m.MountPath,
				ReadOnly:  m.ReadOnly,
				SubPath:   m.SubPath,
				FsMode:    m.FSMode,
			}
			if m.FSUser != nil {
				pv.FsUser = utils.ToInt32NonNegative(*m.FSUser)
				pv.FsUserSet = true
			}
			if m.FSGroup != nil {
				pv.FsGroup = utils.ToInt32NonNegative(*m.FSGroup)
				pv.FsGroupSet = true
			}
			if m.Claim != nil {
				pv.Claim = &generated.VolumeClaim{Name: m.Claim.Name}
			}
			if m.ClaimTemplate != nil {
				pv.ClaimTemplate = &generated.VolumeClaimTemplate{
					StorageClassName: m.ClaimTemplate.StorageClassName,
					Size:             m.ClaimTemplate.Size,
					AccessMode:       string(m.ClaimTemplate.AccessMode),
					Parameters:       m.ClaimTemplate.Parameters,
					ReclaimPolicy:    string(m.ClaimTemplate.ReclaimPolicy),
				}
			}
			protoService.Volumes[i] = pv
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
				Host:                service.Health.Liveness.Host,
			}

			switch service.Health.Liveness.Type {
			case "http":
				protoService.Health.Liveness.Type = generated.ProbeType_PROBE_TYPE_HTTP
				protoService.Health.Liveness.Path = service.Health.Liveness.Path
				protoService.Health.Liveness.Port = utils.ToInt32NonNegative(service.Health.Liveness.Port)
			case "tcp":
				protoService.Health.Liveness.Type = generated.ProbeType_PROBE_TYPE_TCP
				protoService.Health.Liveness.Port = utils.ToInt32NonNegative(service.Health.Liveness.Port)
			case "exec":
				protoService.Health.Liveness.Type = generated.ProbeType_PROBE_TYPE_EXEC
				protoService.Health.Liveness.Command = service.Health.Liveness.Command
			}
		}

		if service.Health.Readiness != nil {
			protoService.Health.Readiness = &generated.Probe{
				InitialDelaySeconds: utils.ToInt32NonNegative(service.Health.Readiness.InitialDelaySeconds),
				PeriodSeconds:       utils.ToInt32NonNegative(service.Health.Readiness.IntervalSeconds),
				TimeoutSeconds:      utils.ToInt32NonNegative(service.Health.Readiness.TimeoutSeconds),
				Host:                service.Health.Readiness.Host,
			}

			switch service.Health.Readiness.Type {
			case "http":
				protoService.Health.Readiness.Type = generated.ProbeType_PROBE_TYPE_HTTP
				protoService.Health.Readiness.Path = service.Health.Readiness.Path
				protoService.Health.Readiness.Port = utils.ToInt32NonNegative(service.Health.Readiness.Port)
			case "tcp":
				protoService.Health.Readiness.Type = generated.ProbeType_PROBE_TYPE_TCP
				protoService.Health.Readiness.Port = utils.ToInt32NonNegative(service.Health.Readiness.Port)
			case "exec":
				protoService.Health.Readiness.Type = generated.ProbeType_PROBE_TYPE_EXEC
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

	// InitSteps (RUNE-121) and main-container SecurityContext.
	if len(service.InitSteps) > 0 {
		protoService.InitSteps = make([]*generated.InitStep, 0, len(service.InitSteps))
		for i := range service.InitSteps {
			protoService.InitSteps = append(protoService.InitSteps, initStepToProto(&service.InitSteps[i]))
		}
	}
	if service.SecurityContext != nil {
		protoService.SecurityContext = securityContextToProto(service.SecurityContext)
	}

	// Instances are populated by GetService / ListServices so callers
	// (notably `rune cast`'s readiness wait) can see backing-instance
	// state in a single round-trip. Read-only on the wire — Create/Update
	// ignore this field server-side.
	if len(service.Instances) > 0 {
		protoService.Instances = make([]*generated.Instance, 0, len(service.Instances))
		for i := range service.Instances {
			protoService.Instances = append(protoService.Instances, embeddedInstanceToProto(&service.Instances[i]))
		}
	}

	if service.Discovery != nil {
		protoService.Discovery = &generated.ServiceDiscovery{
			Vip:                service.Discovery.VIP,
			Mode:               service.Discovery.Mode,
			LocalityPreference: service.Discovery.LocalityPreference,
		}
	}

	if service.IngressCert != nil {
		ic := service.IngressCert
		protoService.IngressCert = &generated.IngressCertStatus{
			Host:      ic.Host,
			State:     string(ic.State),
			IssuedAt:  formatProtoTime(ic.IssuedAt),
			ExpiresAt: formatProtoTime(ic.ExpiresAt),
			LastError: ic.LastError,
			NextRetry: formatProtoTime(ic.NextRetry),
		}
	}

	if service.NetworkPolicy != nil {
		protoService.NetworkPolicy = networkPolicyToProto(service.NetworkPolicy)
	}

	return protoService
}

// formatProtoTime renders an optional time as an RFC 3339 string ("" if nil),
// matching the string-timestamp convention used across the API surface.
func formatProtoTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// networkPolicyFromProto is the inverse of networkPolicyToProto.
func networkPolicyFromProto(np *generated.ServiceNetworkPolicy) *types.ServiceNetworkPolicy {
	out := &types.ServiceNetworkPolicy{}
	peers := func(in []*generated.NetworkPolicyPeer) []types.NetworkPolicyPeer {
		ps := make([]types.NetworkPolicyPeer, 0, len(in))
		for _, p := range in {
			ps = append(ps, types.NetworkPolicyPeer{
				Service:         p.GetService(),
				Namespace:       p.GetNamespace(),
				ServiceSelector: p.GetServiceSelector(),
				CIDR:            p.GetCidr(),
			})
		}
		return ps
	}
	for _, r := range np.GetIngress() {
		out.Ingress = append(out.Ingress, types.IngressRule{From: peers(r.GetFrom()), Ports: r.GetPorts()})
	}
	for _, r := range np.GetEgress() {
		out.Egress = append(out.Egress, types.EgressRule{To: peers(r.GetTo()), Ports: r.GetPorts()})
	}
	return out
}

// networkPolicyToProto converts the embedded service network policy to its
// wire form. Peers carry whichever selector the operator set (service,
// namespace, serviceSelector, or cidr).
func networkPolicyToProto(np *types.ServiceNetworkPolicy) *generated.ServiceNetworkPolicy {
	out := &generated.ServiceNetworkPolicy{}
	peers := func(in []types.NetworkPolicyPeer) []*generated.NetworkPolicyPeer {
		ps := make([]*generated.NetworkPolicyPeer, 0, len(in))
		for i := range in {
			ps = append(ps, &generated.NetworkPolicyPeer{
				Service:         in[i].Service,
				Namespace:       in[i].Namespace,
				ServiceSelector: in[i].ServiceSelector,
				Cidr:            in[i].CIDR,
			})
		}
		return ps
	}
	for i := range np.Ingress {
		out.Ingress = append(out.Ingress, &generated.NetworkPolicyIngressRule{
			From:  peers(np.Ingress[i].From),
			Ports: np.Ingress[i].Ports,
		})
	}
	for i := range np.Egress {
		out.Egress = append(out.Egress, &generated.NetworkPolicyEgressRule{
			To:    peers(np.Egress[i].To),
			Ports: np.Egress[i].Ports,
		})
	}
	return out
}

func ProtoToService(proto *generated.Service) (*types.Service, error) {
	if proto == nil {
		return nil, fmt.Errorf("proto service is nil")
	}

	// Create an initial service with basic fields
	service := &types.Service{
		ID:                 proto.Id,
		Name:               proto.Name,
		Namespace:          proto.Namespace,
		Image:              proto.Image,
		Command:            proto.Command,
		Scale:              int(proto.Scale),
		Runtime:            types.RuntimeType(proto.Runtime),
		ImagePull:          proto.ImagePull,
		ImagePullAnonymous: proto.ImagePullAnonymous,
		StatusReason:       proto.StatusReason,
		StatusMessage:      proto.StatusMessage,
		Labels:             proto.Labels,
	}

	// Convert metadata
	if proto.Metadata != nil {
		if service.Metadata == nil {
			service.Metadata = &types.ServiceMetadata{}
		}
		service.Metadata.Generation = proto.Metadata.Generation
		service.Metadata.LastNonZeroScale = int(proto.Metadata.LastNonZeroScale)
		service.Metadata.TemplateGeneration = proto.Metadata.TemplateGeneration
		service.Metadata.ObservedGeneration = proto.Metadata.ObservedGeneration

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

	// Update strategy + drain grace (RUNE-042).
	if proto.UpdateStrategy != nil {
		service.UpdateStrategy = &types.UpdateStrategy{
			Type: types.UpdateStrategyType(proto.UpdateStrategy.Type),
		}
	}
	if proto.DrainSeconds > 0 {
		drain := int(proto.DrainSeconds)
		service.DrainSeconds = &drain
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
				HostPort:   int(port.HostPort),
			}
		}
	}

	// Convert expose
	if proto.Expose != nil {
		service.Expose = &types.ServiceExpose{
			Port:       proto.Expose.Port,
			Host:       proto.Expose.Host,
			Path:       proto.Expose.Path,
			AllowCIDRs: proto.Expose.AllowCidrs,
		}
		if proto.Expose.Tls != nil {
			service.Expose.TLS = &types.ExposeServiceTLS{
				Secret: proto.Expose.Tls.Secret,
				Auto:   proto.Expose.Tls.Auto,
				Mode:   proto.Expose.Tls.Mode,
			}
		}
		if proto.Expose.ClientCert != nil {
			service.Expose.ClientCert = &types.ExposeClientCert{
				CASecret: proto.Expose.ClientCert.CaSecret,
				Mode:     proto.Expose.ClientCert.Mode,
			}
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

	// Convert volume mounts.
	if len(proto.Volumes) > 0 {
		service.Volumes = make([]types.VolumeMount, len(proto.Volumes))
		for i, m := range proto.Volumes {
			vm := types.VolumeMount{
				Name:      m.Name,
				MountPath: m.MountPath,
				ReadOnly:  m.ReadOnly,
				SubPath:   m.SubPath,
				FSMode:    m.FsMode,
			}
			if m.FsUserSet {
				u := int(m.FsUser)
				vm.FSUser = &u
			}
			if m.FsGroupSet {
				g := int(m.FsGroup)
				vm.FSGroup = &g
			}
			if m.Claim != nil {
				vm.Claim = &types.VolumeClaim{Name: m.Claim.Name}
			}
			if m.ClaimTemplate != nil {
				vm.ClaimTemplate = &types.VolumeClaimTemplate{
					StorageClassName: m.ClaimTemplate.StorageClassName,
					Size:             m.ClaimTemplate.Size,
					AccessMode:       types.AccessMode(m.ClaimTemplate.AccessMode),
					Parameters:       m.ClaimTemplate.Parameters,
					ReclaimPolicy:    types.ReclaimPolicy(m.ClaimTemplate.ReclaimPolicy),
				}
			}
			service.Volumes[i] = vm
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
				Host:                proto.Health.Liveness.Host,
			}

			switch proto.Health.Liveness.Type {
			case generated.ProbeType_PROBE_TYPE_HTTP:
				service.Health.Liveness.Type = "http"
				service.Health.Liveness.Path = proto.Health.Liveness.Path
				service.Health.Liveness.Port = int(proto.Health.Liveness.Port)
			case generated.ProbeType_PROBE_TYPE_TCP:
				service.Health.Liveness.Type = "tcp"
				service.Health.Liveness.Port = int(proto.Health.Liveness.Port)
			case generated.ProbeType_PROBE_TYPE_EXEC:
				service.Health.Liveness.Type = "exec"
				service.Health.Liveness.Command = proto.Health.Liveness.Command
			}
		}

		if proto.Health.Readiness != nil {
			service.Health.Readiness = &types.Probe{
				InitialDelaySeconds: int(proto.Health.Readiness.InitialDelaySeconds),
				IntervalSeconds:     int(proto.Health.Readiness.PeriodSeconds),
				TimeoutSeconds:      int(proto.Health.Readiness.TimeoutSeconds),
				Host:                proto.Health.Readiness.Host,
			}

			switch proto.Health.Readiness.Type {
			case generated.ProbeType_PROBE_TYPE_HTTP:
				service.Health.Readiness.Type = "http"
				service.Health.Readiness.Path = proto.Health.Readiness.Path
				service.Health.Readiness.Port = int(proto.Health.Readiness.Port)
			case generated.ProbeType_PROBE_TYPE_TCP:
				service.Health.Readiness.Type = "tcp"
				service.Health.Readiness.Port = int(proto.Health.Readiness.Port)
			case generated.ProbeType_PROBE_TYPE_EXEC:
				service.Health.Readiness.Type = "exec"
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

	// InitSteps (RUNE-121) and main-container SecurityContext.
	if len(proto.InitSteps) > 0 {
		service.InitSteps = make([]types.InitStep, 0, len(proto.InitSteps))
		for _, p := range proto.InitSteps {
			service.InitSteps = append(service.InitSteps, initStepFromProto(p))
		}
	}
	if proto.SecurityContext != nil {
		service.SecurityContext = securityContextFromProto(proto.SecurityContext)
	}

	if proto.Discovery != nil {
		service.Discovery = &types.ServiceDiscovery{
			VIP:                proto.Discovery.Vip,
			Mode:               proto.Discovery.Mode,
			LocalityPreference: proto.Discovery.LocalityPreference,
		}
	}

	// IngressCert is read-only status (server-populated), so there is no reverse
	// mapping for it here. NetworkPolicy is spec — round-trip it so a service
	// created/updated through the proto API keeps its policy.
	if proto.NetworkPolicy != nil {
		service.NetworkPolicy = networkPolicyFromProto(proto.NetworkPolicy)
	}

	if len(proto.Instances) > 0 {
		service.Instances = make([]types.Instance, 0, len(proto.Instances))
		for _, pi := range proto.Instances {
			inst := embeddedInstanceFromProto(pi)
			if inst != nil {
				service.Instances = append(service.Instances, *inst)
			}
		}
	}

	return service, nil
}

// embeddedInstanceToProto / embeddedInstanceFromProto are the minimal
// converters used when an Instance is carried inside a Service response
// (GetService, ListServices). They cover the fields the CLI reads off
// service.Instances — primarily Status/Name/ID/StatusMessage. Callers
// needing the full instance object should still use ListInstances; this
// embed avoids a second round-trip for readiness checks but isn't a
// drop-in replacement for the dedicated instance RPCs.
func embeddedInstanceToProto(i *types.Instance) *generated.Instance {
	if i == nil {
		return nil
	}
	return &generated.Instance{
		Id:            i.ID,
		Runner:        string(i.Runner),
		Namespace:     i.Namespace,
		Name:          i.Name,
		ServiceId:     i.ServiceID,
		ServiceName:   i.ServiceName,
		NodeId:        i.NodeID,
		Labels:        i.Labels,
		Ip:            i.IP,
		Status:        instanceStatusToProto(i.Status),
		StatusMessage: i.StatusMessage,
		ContainerId:   i.ContainerID,
		Pid:           utils.ToInt32NonNegative(i.PID),
		CreatedAt:     formatInstanceTime(i.CreatedAt),
		UpdatedAt:     formatInstanceTime(i.UpdatedAt),
		Usage:         InstanceUsageToProto(i.Usage),
	}
}

// InstanceUsageToProto maps the transient live-usage sample onto the wire.
// nil in → nil out: an absent usage field means "unknown", which clients
// must render as unknown rather than 0.
func InstanceUsageToProto(u *types.InstanceUsage) *generated.InstanceUsage {
	if u == nil {
		return nil
	}
	return &generated.InstanceUsage{
		CpuPercent:    u.CPUPercent,
		MemUsedBytes:  u.MemUsedBytes,
		MemLimitBytes: u.MemLimitBytes,
	}
}

// formatInstanceTime renders a timestamp as RFC3339, or "" for the zero
// value so inlined instances don't serialize "0001-01-01T00:00:00Z".
func formatInstanceTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func embeddedInstanceFromProto(p *generated.Instance) *types.Instance {
	if p == nil {
		return nil
	}
	inst := &types.Instance{
		ID:            p.Id,
		Runner:        types.RunnerType(p.Runner),
		Namespace:     p.Namespace,
		Name:          p.Name,
		ServiceID:     p.ServiceId,
		ServiceName:   p.ServiceName,
		NodeID:        p.NodeId,
		Labels:        p.Labels,
		IP:            p.Ip,
		Status:        instanceStatusFromProto(p.Status),
		StatusMessage: p.StatusMessage,
		ContainerID:   p.ContainerId,
		PID:           int(p.Pid),
	}
	if ts, err := parseTimestamp(p.CreatedAt); err == nil && ts != nil {
		inst.CreatedAt = *ts
	}
	if ts, err := parseTimestamp(p.UpdatedAt); err == nil && ts != nil {
		inst.UpdatedAt = *ts
	}
	return inst
}

func instanceStatusToProto(s types.InstanceStatus) generated.InstanceStatus {
	switch s {
	case types.InstanceStatusPending:
		return generated.InstanceStatus_INSTANCE_STATUS_PENDING
	case types.InstanceStatusCreated:
		return generated.InstanceStatus_INSTANCE_STATUS_CREATED
	case types.InstanceStatusStarting:
		return generated.InstanceStatus_INSTANCE_STATUS_STARTING
	case types.InstanceStatusRunning:
		return generated.InstanceStatus_INSTANCE_STATUS_RUNNING
	case types.InstanceStatusStopped:
		return generated.InstanceStatus_INSTANCE_STATUS_STOPPED
	case types.InstanceStatusFailed:
		return generated.InstanceStatus_INSTANCE_STATUS_FAILED
	case types.InstanceStatusExited:
		return generated.InstanceStatus_INSTANCE_STATUS_EXITED
	case types.InstanceStatusDeleted:
		return generated.InstanceStatus_INSTANCE_STATUS_DELETED
	default:
		return generated.InstanceStatus_INSTANCE_STATUS_PENDING
	}
}

func instanceStatusFromProto(s generated.InstanceStatus) types.InstanceStatus {
	switch s {
	case generated.InstanceStatus_INSTANCE_STATUS_PENDING:
		return types.InstanceStatusPending
	case generated.InstanceStatus_INSTANCE_STATUS_CREATED:
		return types.InstanceStatusCreated
	case generated.InstanceStatus_INSTANCE_STATUS_STARTING:
		return types.InstanceStatusStarting
	case generated.InstanceStatus_INSTANCE_STATUS_RUNNING:
		return types.InstanceStatusRunning
	case generated.InstanceStatus_INSTANCE_STATUS_STOPPED:
		return types.InstanceStatusStopped
	case generated.InstanceStatus_INSTANCE_STATUS_FAILED:
		return types.InstanceStatusFailed
	case generated.InstanceStatus_INSTANCE_STATUS_EXITED:
		return types.InstanceStatusExited
	case generated.InstanceStatus_INSTANCE_STATUS_DELETED:
		return types.InstanceStatusDeleted
	default:
		return types.InstanceStatusPending
	}
}

// initStepToProto converts a types.InitStep to its proto form.
func initStepToProto(s *types.InitStep) *generated.InitStep {
	if s == nil {
		return nil
	}
	p := &generated.InitStep{
		Name:          s.Name,
		Image:         s.Image,
		Command:       s.Command,
		Args:          append([]string(nil), s.Args...),
		Env:           cloneStringMap(s.Env),
		TimeoutNanos:  int64(s.Timeout),
		RestartPolicy: string(s.RestartPolicy),
	}
	for _, src := range s.EnvFrom {
		p.EnvFrom = append(p.EnvFrom, &generated.EnvFromSource{
			SecretName:    src.SecretName,
			ConfigmapName: src.ConfigmapName,
			Namespace:     src.Namespace,
			Prefix:        src.Prefix,
		})
	}
	// Preserve nil vs empty-slice distinction (see types.InitStep docs).
	if s.Volumes != nil {
		p.VolumesSet = true
		p.Volumes = append([]string(nil), s.Volumes...)
	}
	if s.SecretMounts != nil {
		p.SecretMountsSet = true
		p.SecretMounts = append([]string(nil), s.SecretMounts...)
	}
	if s.ConfigmapMounts != nil {
		p.ConfigmapMountsSet = true
		p.ConfigmapMounts = append([]string(nil), s.ConfigmapMounts...)
	}
	if s.Resources != nil {
		p.Resources = &generated.Resources{
			Cpu: &generated.ResourceLimit{
				Request: s.Resources.CPU.Request,
				Limit:   s.Resources.CPU.Limit,
			},
			Memory: &generated.ResourceLimit{
				Request: s.Resources.Memory.Request,
				Limit:   s.Resources.Memory.Limit,
			},
		}
	}
	if s.RunIf.Type != "" || s.RunIf.Path != "" || s.RunIf.Volume != "" {
		p.RunIf = &generated.RunIfSpec{
			Type:   string(s.RunIf.Type),
			Path:   s.RunIf.Path,
			Volume: s.RunIf.Volume,
		}
	}
	if s.SecurityContext != nil {
		p.SecurityContext = securityContextToProto(s.SecurityContext)
	}
	return p
}

func initStepFromProto(p *generated.InitStep) types.InitStep {
	s := types.InitStep{
		Name:          p.Name,
		Image:         p.Image,
		Command:       p.Command,
		Args:          append([]string(nil), p.Args...),
		Env:           cloneStringMap(p.Env),
		Timeout:       time.Duration(p.TimeoutNanos),
		RestartPolicy: types.InitStepRestartPolicy(p.RestartPolicy),
	}
	for _, src := range p.EnvFrom {
		s.EnvFrom = append(s.EnvFrom, types.EnvFromSource{
			SecretName:    src.SecretName,
			ConfigmapName: src.ConfigmapName,
			Namespace:     src.Namespace,
			Prefix:        src.Prefix,
		})
	}
	if p.VolumesSet {
		s.Volumes = append([]string{}, p.Volumes...)
	}
	if p.SecretMountsSet {
		s.SecretMounts = append([]string{}, p.SecretMounts...)
	}
	if p.ConfigmapMountsSet {
		s.ConfigmapMounts = append([]string{}, p.ConfigmapMounts...)
	}
	if p.Resources != nil {
		r := &types.Resources{}
		if p.Resources.Cpu != nil {
			r.CPU.Request = p.Resources.Cpu.Request
			r.CPU.Limit = p.Resources.Cpu.Limit
		}
		if p.Resources.Memory != nil {
			r.Memory.Request = p.Resources.Memory.Request
			r.Memory.Limit = p.Resources.Memory.Limit
		}
		s.Resources = r
	}
	if p.RunIf != nil {
		s.RunIf = types.RunIf{
			Type:   types.RunIfType(p.RunIf.Type),
			Path:   p.RunIf.Path,
			Volume: p.RunIf.Volume,
		}
	}
	if p.SecurityContext != nil {
		s.SecurityContext = securityContextFromProto(p.SecurityContext)
	}
	return s
}

func securityContextToProto(sc *types.SecurityContext) *generated.SecurityContext {
	if sc == nil {
		return nil
	}
	out := &generated.SecurityContext{
		CapAdd:     append([]string(nil), sc.CapAdd...),
		CapDrop:    append([]string(nil), sc.CapDrop...),
		Privileged: sc.Privileged,
	}
	if sc.SeccompProfile != nil {
		out.SeccompProfile = &generated.SeccompProfile{
			Type:             string(sc.SeccompProfile.Type),
			LocalhostProfile: sc.SeccompProfile.LocalhostProfile,
		}
	}
	return out
}

func securityContextFromProto(p *generated.SecurityContext) *types.SecurityContext {
	if p == nil {
		return nil
	}
	sc := &types.SecurityContext{
		CapAdd:     append([]string(nil), p.CapAdd...),
		CapDrop:    append([]string(nil), p.CapDrop...),
		Privileged: p.Privileged,
	}
	if p.SeccompProfile != nil {
		sc.SeccompProfile = &types.SeccompProfile{
			Type:             types.SeccompProfileType(p.SeccompProfile.Type),
			LocalhostProfile: p.SeccompProfile.LocalhostProfile,
		}
	}
	return sc
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
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
