package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// DefaultNamespace is the default namespace for services.
	DefaultNamespace = "default"
)

// ServiceService implements the gRPC ServiceService.
type ServiceService struct {
	generated.UnimplementedServiceServiceServer

	namespaceRepo *repos.NamespaceRepo
	orchestrator  orchestrator.Orchestrator
	runnerManager *manager.RunnerManager
	logger        log.Logger
}

// NewServiceService creates a new ServiceService with the given orchestrator and logger.
func NewServiceService(store store.Store, orchestrator orchestrator.Orchestrator, runnerManager *manager.RunnerManager, logger log.Logger) *ServiceService {
	return &ServiceService{
		namespaceRepo: repos.NewNamespaceRepo(store),
		orchestrator:  orchestrator,
		runnerManager: runnerManager,
		logger:        logger,
	}
}

// CreateService creates a new service.
func (s *ServiceService) CreateService(ctx context.Context, req *generated.CreateServiceRequest) (*generated.ServiceResponse, error) {
	s.logger.Debug("CreateService called")

	if req.Service == nil {
		return nil, status.Error(codes.InvalidArgument, "service is required")
	}

	// Convert protobuf message to domain model
	service, err := client.ProtoToService(req.Service)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid service: %v", err)
	}

	// Generate service ID if not provided
	if service.ID == "" {
		service.ID = uuid.New().String()
	}

	// Set namespace to default if not provided and validate that it exists
	service.Namespace = types.NS(service.Namespace)
	if err := ensureNamespaceExists(ctx, s.namespaceRepo, service.Namespace, req.EnsureNamespace); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "failed to ensure namespace exists: %v", err)
	}

	//Set Metadata with initial generation, creation and update times
	now := time.Now()
	last := service.Scale
	if last < 1 {
		last = 1
	}
	service.Metadata = &types.ServiceMetadata{
		Generation:       1,
		CreatedAt:        now,
		UpdatedAt:        now,
		LastNonZeroScale: last,
	}

	// Set initial status
	service.Status = types.ServiceStatusPending

	// Validate global dependency cycles including this new service
	if err := s.validateGlobalDependencyCycles(ctx, service); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "dependency validation failed: %v", err)
	}

	// If service uses container runtime, ensure Docker runner is available
	if service.Runtime == "" || service.Runtime == types.RuntimeType("container") {
		if s.runnerManager != nil {
			if _, err := s.runnerManager.GetDockerRunner(); err != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "docker is not available: install Docker and ensure the daemon is running")
			}
		}
	}

	// Use orchestrator to create the service
	if err := s.orchestrator.CreateService(ctx, service); err != nil {
		// If the service already exists, fall back to the ServiceService.UpdateService
		if store.IsAlreadyExistsError(err) {
			s.logger.Info("Service already exists, updating instead",
				log.Str("name", service.Name),
				log.Str("namespace", service.Namespace))
			// Reuse the original request's proto for update so we go through the
			// full UpdateService pipeline (hash/generation logic, etc.)
			return s.UpdateService(ctx, &generated.UpdateServiceRequest{
				Service:       req.Service,
				DeploymentTag: req.DeploymentTag,
				Force:         false,
			})
		}
		s.logger.Error("Failed to create service", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to create service: %v", err)
	}

	// Convert back to protobuf message
	return &generated.ServiceResponse{
		Service: client.ServiceToProto(service),
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: "Service created successfully",
		},
	}, nil
}

// GetService retrieves a service by name.
func (s *ServiceService) GetService(ctx context.Context, req *generated.GetServiceRequest) (*generated.ServiceResponse, error) {
	s.logger.Debug("GetService called", log.Str("name", req.Name))

	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "service name is required")
	}

	namespace := req.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}

	// Get the service from orchestrator
	s.logger.Debug("Getting service", log.Str("namespace", namespace), log.Str("name", req.Name))
	service, err := s.orchestrator.GetService(ctx, namespace, req.Name)
	if err != nil {
		if IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "service not found: %s", req.Name)
		}
		s.logger.Error("Failed to get service", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to get service: %v", err)
	}

	// Convert to protobuf message
	return &generated.ServiceResponse{
		Service: client.ServiceToProto(service),
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: "Service retrieved successfully",
		},
	}, nil
}

// ListServices lists services with optional filtering.
func (s *ServiceService) ListServices(ctx context.Context, req *generated.ListServicesRequest) (*generated.ListServicesResponse, error) {
	s.logger.Debug("ListServices called")

	namespace := req.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}

	// Handle all namespaces case
	if namespace == "*" {
		namespace = "" // Empty string for all namespaces
	}

	// Get services from orchestrator
	services, err := s.orchestrator.ListServices(ctx, namespace)
	if err != nil {
		s.logger.Error("Failed to list services", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to list services: %v", err)
	}

	s.logger.Debug("Found services", log.Int("count", len(services)))

	// Convert to protobuf messages and apply filtering
	protoServices := make([]*generated.Service, 0, len(services))
	for _, service := range services {
		// Apply selector filtering (both labels and fields)
		if !matchSelectors(service, req.LabelSelector, req.FieldSelector) {
			continue
		}

		protoService := client.ServiceToProto(service)
		if err != nil {
			s.logger.Error("Failed to convert service to proto", log.Err(err))
			continue
		}

		protoServices = append(protoServices, protoService)
	}

	s.logger.Debug("Found proto services", log.Int("count", len(protoServices)))

	return &generated.ListServicesResponse{
		Services: protoServices,
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: fmt.Sprintf("Found %d services", len(protoServices)),
		},
		Paging: &generated.PagingParams{
			Limit:  utils.ToInt32NonNegative(len(protoServices)),
			Offset: 0,
		},
	}, nil
}

// matchSelectors checks if service matches all the labels and fields in the selectors
func matchSelectors(service *types.Service, labels map[string]string, fields map[string]string) bool {
	// Check label selectors
	if len(labels) > 0 {
		// Check if the service has all the requested labels
		for key, value := range labels {
			// If service has no labels or the specific label is not found
			if service.Labels == nil {
				return false
			}

			serviceValue, exists := service.Labels[key]
			if !exists || serviceValue != value {
				return false
			}
		}
	}

	// Check field selectors
	for k, v := range fields {
		switch k {
		case "name":
			if service.Name != v {
				return false
			}
		case "namespace":
			if service.Namespace != v {
				return false
			}
		case "status":
			if string(service.Status) != v {
				return false
			}
		case "runtime":
			if string(service.Runtime) != v {
				return false
			}
		default:
			// Unknown field, consider it a non-match
			return false
		}
	}

	// Match if we passed all checks
	return true
}

// UpdateService updates an existing service.
func (s *ServiceService) UpdateService(ctx context.Context, req *generated.UpdateServiceRequest) (*generated.ServiceResponse, error) {
	s.logger.Debug("UpdateService called")

	if req.Service == nil {
		return nil, status.Error(codes.InvalidArgument, "service is required")
	}

	namespace := req.Service.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}

	// Get the existing service from orchestrator
	existingService, err := s.orchestrator.GetService(ctx, namespace, req.Service.Name)
	if err != nil {
		if IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "service not found: %s", req.Service.Name)
		}
		s.logger.Error("Failed to get service", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to get service: %v", err)
	}

	// Convert protobuf message to domain model
	updatedService, err := client.ProtoToService(req.Service)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid service: %v", err)
	}

	// Preserve the ID, creation time, and other fields that shouldn't change
	updatedService.ID = existingService.ID
	if existingService.Metadata == nil {
		existingService.Metadata = &types.ServiceMetadata{}
	}
	updatedService.Metadata = existingService.Metadata
	updatedService.Metadata.UpdatedAt = time.Now()

	// Determine if we need to increment the generation
	needsGenUpdate := req.Force // Force flag forces generation update

	if !needsGenUpdate {
		// Calculate service hash to determine if anything meaningful changed
		oldHash := existingService.CalculateHash()
		newHash := updatedService.CalculateHash()

		if oldHash != newHash {
			// Service has changed, increment generation
			needsGenUpdate = true
			s.logger.Debug("Service hash changed, incrementing generation",
				log.Str("service", updatedService.Name),
				log.Str("namespace", updatedService.Namespace),
				log.Str("old_hash", oldHash[:8]),
				log.Str("new_hash", newHash[:8]),
				log.Int64("from_generation", existingService.Metadata.Generation),
				log.Int64("to_generation", existingService.Metadata.Generation+1))
		} else {
			// No changes detected, keep existing generation
			s.logger.Debug("Service unchanged, keeping generation",
				log.Str("service", updatedService.Name),
				log.Str("namespace", updatedService.Namespace),
				log.Str("hash", oldHash[:8]),
				log.Int64("generation", existingService.Metadata.Generation))
		}
	} else {
		s.logger.Info("Force flag set, incrementing generation",
			log.Str("service", updatedService.Name),
			log.Str("namespace", updatedService.Namespace),
			log.Int64("from_generation", existingService.Metadata.Generation),
			log.Int64("to_generation", existingService.Metadata.Generation+1))
	}

	// Validate global dependency cycles including the updated service
	if err := s.validateGlobalDependencyCycles(ctx, updatedService); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "dependency validation failed: %v", err)
	}

	if needsGenUpdate {
		// Increment generation to trigger reconciliation
		if updatedService.Metadata == nil {
			updatedService.Metadata = &types.ServiceMetadata{}
		}
		updatedService.Metadata.Generation = existingService.Metadata.Generation + 1
		updatedService.Status = types.ServiceStatusDeploying
	} else {
		// Keep existing generation and status
		if updatedService.Metadata == nil {
			updatedService.Metadata = &types.ServiceMetadata{}
		}
		updatedService.Metadata.Generation = existingService.Metadata.Generation
		updatedService.Status = existingService.Status
	}

	// Use orchestrator to update the service
	if err := s.orchestrator.UpdateService(ctx, updatedService); err != nil {
		s.logger.Error("Failed to update service", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to update service: %v", err)
	}

	// Convert back to protobuf message
	return &generated.ServiceResponse{
		Service: client.ServiceToProto(updatedService),
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: "Service updated successfully",
		},
	}, nil
}

// validateGlobalDependencyCycles ensures that adding/updating the provided service
// does not introduce dependency cycles across all services stored in the system.
func (s *ServiceService) validateGlobalDependencyCycles(ctx context.Context, candidate *types.Service) error {
	// List all services across all namespaces
	existing, err := s.orchestrator.ListServices(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	// Build presence and adjacency including candidate's dependencies
	present := make(map[string]map[string]bool)
	adj := make(map[string][]string)

	// Index existing services and edges
	for _, svc := range existing {
		ns := svc.Namespace
		if ns == "" {
			ns = DefaultNamespace
		}
		if _, ok := present[ns]; !ok {
			present[ns] = make(map[string]bool)
		}
		present[ns][svc.Name] = true
		from := types.MakeDependencyNodeKey(ns, svc.Name)
		for _, d := range svc.Dependencies {
			depNS := d.Namespace
			if depNS == "" {
				depNS = ns
			}
			adj[from] = append(adj[from], types.MakeDependencyNodeKey(depNS, d.Service))
		}
	}

	// Add/replace candidate node and its edges
	cns := candidate.Namespace
	if cns == "" {
		cns = DefaultNamespace
	}
	if _, ok := present[cns]; !ok {
		present[cns] = make(map[string]bool)
	}
	present[cns][candidate.Name] = true
	cfrom := types.MakeDependencyNodeKey(cns, candidate.Name)
	adj[cfrom] = nil // reset edges for candidate
	for _, d := range candidate.Dependencies {
		depNS := d.Namespace
		if depNS == "" {
			depNS = cns
		}
		adj[cfrom] = append(adj[cfrom], types.MakeDependencyNodeKey(depNS, d.Service))
	}

	// Run shared cycle detection
	if errs := types.DetectDependencyCycles(adj); len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// DeleteService removes a service.
func (s *ServiceService) DeleteService(ctx context.Context, req *generated.DeleteServiceRequest) (*generated.DeleteServiceResponse, error) {
	s.logger.Debug("DeleteService called", log.Str("name", req.Name))

	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "service name is required")
	}

	namespace := req.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}

	// Check if the service exists (unless ignore_not_found is set)
	if !req.IgnoreNotFound {
		_, err := s.orchestrator.GetService(ctx, namespace, req.Name)
		if err != nil {
			if IsNotFound(err) {
				return nil, status.Errorf(codes.NotFound, "service not found: %s", req.Name)
			}
			s.logger.Error("Failed to get service", log.Err(err))
			return nil, status.Errorf(codes.Internal, "failed to get service: %v", err)
		}
	}

	// If not forced, block deletion when dependents exist
	if !req.Force {
		dependents, err := s.findDependents(ctx, namespace, req.Name)
		if err != nil {
			s.logger.Error("Failed to check dependents", log.Err(err))
			return nil, status.Errorf(codes.Internal, "failed to check dependents: %v", err)
		}
		if len(dependents) > 0 {
			names := make([]string, 0, len(dependents))
			for _, d := range dependents {
				names = append(names, fmt.Sprintf("%s/%s", d.Namespace, d.Name))
			}
			return nil, status.Errorf(codes.FailedPrecondition, "cannot delete %s/%s; dependents exist: %s (use --no-dependencies to override)", namespace, req.Name, strings.Join(names, ", "))
		}
	}

	// Create deletion request
	deletionRequest := &types.DeletionRequest{
		Name:           req.Name,
		Namespace:      namespace,
		Force:          req.Force,
		TimeoutSeconds: req.TimeoutSeconds,
		Detach:         req.Detach,
		DryRun:         req.DryRun,
		GracePeriod:    req.GracePeriod,
		Now:            req.Now,
		IgnoreNotFound: req.IgnoreNotFound,
		Finalizers:     req.Finalizers,
	}

	// Use the deletion controller to handle the deletion
	if s.orchestrator == nil {
		return nil, status.Error(codes.Internal, "deletion controller not available")
	}

	deletionResponse, err := s.orchestrator.DeleteService(ctx, deletionRequest)
	if err != nil {
		s.logger.Error("Failed to delete service", log.Err(err), log.Str("name", req.Name))
		return nil, status.Errorf(codes.Internal, "failed to delete service: %v", err)
	}

	// Convert the deletion response to the gRPC response
	response := &generated.DeleteServiceResponse{
		DeletionId: deletionResponse.DeletionID,
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: deletionResponse.Status,
		},
		Warnings:   deletionResponse.Warnings,
		Errors:     deletionResponse.Errors,
		Finalizers: convertFinalizersToProto(deletionResponse.Finalizers),
	}

	return response, nil
}

// findDependents returns services that declare a dependency on target namespace/name
func (s *ServiceService) findDependents(ctx context.Context, targetNamespace, targetName string) ([]*types.Service, error) {
	// List across all namespaces
	services, err := s.orchestrator.ListServices(ctx, "")
	if err != nil {
		return nil, err
	}
	var result []*types.Service
	for _, svc := range services {
		for _, dep := range svc.Dependencies {
			ns := dep.Namespace
			if ns == "" {
				ns = svc.Namespace
			}
			if ns == targetNamespace && dep.Service == targetName {
				result = append(result, svc)
				break
			}
		}
	}
	return result, nil
}

// convertFinalizersToProto converts domain finalizers to protobuf finalizers
func convertFinalizersToProto(finalizers []types.Finalizer) []*generated.Finalizer {
	result := make([]*generated.Finalizer, len(finalizers))
	for i, finalizer := range finalizers {
		protoFinalizer := &generated.Finalizer{
			Id:        finalizer.ID,
			Type:      string(finalizer.Type),
			Status:    string(finalizer.Status),
			Error:     finalizer.Error,
			CreatedAt: finalizer.CreatedAt.Unix(),
			UpdatedAt: finalizer.UpdatedAt.Unix(),
		}

		// Set completion time if not nil
		if finalizer.CompletedAt != nil {
			protoFinalizer.CompletedAt = finalizer.CompletedAt.Unix()
		}

		// Convert dependencies
		protoDependencies := make([]*generated.FinalizerDependency, 0, len(finalizer.Dependencies))
		for _, dependency := range finalizer.Dependencies {
			protoDependencies = append(protoDependencies, &generated.FinalizerDependency{
				DependsOn: string(dependency.DependsOn),
				Required:  dependency.Required,
			})
		}
		protoFinalizer.Dependencies = protoDependencies

		result[i] = protoFinalizer
	}
	return result
}

// GetDeletionStatus gets the status of a deletion operation.
func (s *ServiceService) GetDeletionStatus(ctx context.Context, req *generated.GetDeletionStatusRequest) (*generated.GetDeletionStatusResponse, error) {
	s.logger.Debug("GetDeletionStatus called", log.Str("deletion_id", req.Name))

	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "deletion_id is required")
	}

	if s.orchestrator == nil {
		return nil, status.Error(codes.Internal, "orchestrator not available")
	}

	// Get deletion operation from orchestrator
	operation, err := s.orchestrator.GetDeletionStatus(ctx, req.Namespace, req.Name)
	if err != nil {
		s.logger.Error("Failed to get deletion status", log.Err(err), log.Str("deletion_id", req.Name))
		return nil, status.Errorf(codes.Internal, "failed to get deletion status: %v", err)
	}

	if operation == nil {
		return nil, status.Errorf(codes.NotFound, "deletion operation not found: %s", req.Name)
	}

	// Convert to protobuf message
	protoOperation, err := s.deletionOperationToProto(operation)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to convert deletion operation: %v", err)
	}

	return &generated.GetDeletionStatusResponse{
		Operation: protoOperation,
	}, nil
}

// ListDeletionOperations lists deletion operations with optional filtering.
func (s *ServiceService) ListDeletionOperations(ctx context.Context, req *generated.ListDeletionOperationsRequest) (*generated.ListDeletionOperationsResponse, error) {
	s.logger.Debug("ListDeletionOperations called",
		log.Str("namespace", req.Namespace),
		log.Str("status", req.Status))

	// Get deletion operations from orchestrator
	deletionOps, err := s.orchestrator.ListDeletionOperations(ctx, req.Namespace)
	if err != nil {
		s.logger.Error("Failed to list deletion operations", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to list deletion operations: %v", err)
	}

	// Filter by namespace and status
	var filteredOps []*types.DeletionOperation
	for _, op := range deletionOps {
		// Apply namespace filter if we listed from all namespaces
		if req.Namespace != "" && req.Namespace != "*" && op.Namespace != req.Namespace {
			continue
		}

		// Apply status filter if specified
		if req.Status != "" && string(op.Status) != req.Status {
			continue
		}

		// Add to filtered list (op is already a pointer)
		filteredOps = append(filteredOps, op)
	}

	// Convert to protobuf operations
	protoOperations := make([]*generated.DeletionOperation, 0, len(filteredOps))
	for _, op := range filteredOps {
		protoOp, err := s.deletionOperationToProto(op)
		if err != nil {
			s.logger.Error("Failed to convert deletion operation to proto", log.Err(err))
			continue // Skip this operation but continue with others
		}
		protoOperations = append(protoOperations, protoOp)
	}

	s.logger.Debug("Listed deletion operations",
		log.Int("total", len(deletionOps)),
		log.Int("filtered", len(filteredOps)),
		log.Int("returned", len(protoOperations)))

	return &generated.ListDeletionOperationsResponse{
		Operations: protoOperations,
	}, nil
}

// deletionOperationToProto converts a domain model deletion operation to a protobuf message.
func (s *ServiceService) deletionOperationToProto(operation *types.DeletionOperation) (*generated.DeletionOperation, error) {
	if operation == nil {
		return nil, fmt.Errorf("operation is nil")
	}

	protoOperation := &generated.DeletionOperation{
		Id:                operation.ID,
		Namespace:         operation.Namespace,
		ServiceName:       operation.ServiceName,
		TotalInstances:    utils.ToInt32NonNegative(operation.TotalInstances),
		DeletedInstances:  utils.ToInt32NonNegative(operation.DeletedInstances),
		FailedInstances:   utils.ToInt32NonNegative(operation.FailedInstances),
		StartTime:         operation.StartTime.Unix(),
		Status:            string(operation.Status),
		FailureReason:     operation.FailureReason,
		PendingOperations: operation.PendingOperations,
	}

	// Set end time if not nil
	if operation.EndTime != nil {
		protoOperation.EndTime = operation.EndTime.Unix()
	}

	// Set estimated completion time if not nil
	if operation.EstimatedCompletion != nil {
		protoOperation.EstimatedCompletion = operation.EstimatedCompletion.Unix()
	}

	// Convert finalizers
	protoFinalizers := make([]*generated.Finalizer, 0, len(operation.Finalizers))
	for _, finalizer := range operation.Finalizers {
		protoFinalizer := &generated.Finalizer{
			Id:        finalizer.ID,
			Type:      string(finalizer.Type),
			Status:    string(finalizer.Status),
			Error:     finalizer.Error,
			CreatedAt: finalizer.CreatedAt.Unix(),
			UpdatedAt: finalizer.UpdatedAt.Unix(),
		}

		// Set completion time if not nil
		if finalizer.CompletedAt != nil {
			protoFinalizer.CompletedAt = finalizer.CompletedAt.Unix()
		}

		// Convert dependencies
		protoDependencies := make([]*generated.FinalizerDependency, 0, len(finalizer.Dependencies))
		for _, dependency := range finalizer.Dependencies {
			protoDependencies = append(protoDependencies, &generated.FinalizerDependency{
				DependsOn: string(dependency.DependsOn),
				Required:  dependency.Required,
			})
		}
		protoFinalizer.Dependencies = protoDependencies

		protoFinalizers = append(protoFinalizers, protoFinalizer)
	}
	protoOperation.Finalizers = protoFinalizers

	return protoOperation, nil
}

// ScaleService changes the scale of a service.
func (s *ServiceService) ScaleService(ctx context.Context, req *generated.ScaleServiceRequest) (*generated.ServiceResponse, error) {
	s.logger.Debug("ScaleService called", log.Str("name", req.Name), log.Int("scale", int(req.Scale)))

	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "service name is required")
	}

	if req.Scale < 0 {
		return nil, status.Error(codes.InvalidArgument, "scale must be a non-negative integer")
	}

	namespace := req.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}

	// Get the service from orchestrator
	service, err := s.orchestrator.GetService(ctx, namespace, req.Name)
	if err != nil {
		if IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "service not found: %s", req.Name)
		}
		s.logger.Error("Failed to get service", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to get service: %v", err)
	}

	// Track the current scale and target scale
	currentScale := service.Scale
	targetScale := int(req.Scale)

	// Update last non-zero scale if scaling to >0
	if targetScale > 0 {
		if service.Metadata == nil {
			service.Metadata = &types.ServiceMetadata{}
		}
		if targetScale > service.Metadata.LastNonZeroScale {
			service.Metadata.LastNonZeroScale = targetScale
		}
	}

	// Check if we're already at the target scale
	if currentScale == targetScale {
		// Convert service to proto and return it
		return &generated.ServiceResponse{
			Service: client.ServiceToProto(service),
			Status: &generated.Status{
				Code:    int32(codes.OK),
				Message: fmt.Sprintf("Service already at scale %d", targetScale),
			},
		}, nil
	}

	// Create scaling parameters
	params := types.ScalingOperationParams{
		CurrentScale:    currentScale,
		TargetScale:     targetScale,
		StepSize:        1,  // Default step size
		IntervalSeconds: 30, // Default interval
		IsGradual:       req.Mode == generated.ScalingMode_SCALING_MODE_GRADUAL,
	}

	// Override defaults if gradual scaling is requested
	if params.IsGradual {
		if req.StepSize > 0 {
			params.StepSize = int(req.StepSize)
		}
		if req.IntervalSeconds > 0 {
			params.IntervalSeconds = int(req.IntervalSeconds)
		}
	}

	// Initiate scaling operation; ScalingController will drive service.Scale updates
	err = s.orchestrator.CreateScalingOperation(ctx, service, params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to initiate scaling: %v", err)
	}

	// Convert current service to proto and return it
	return &generated.ServiceResponse{
		Service: client.ServiceToProto(service),
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: fmt.Sprintf("Scaling operation initiated to %d", targetScale),
		},
	}, nil
}

// IsNotFound returns true if the error is a "not found" error.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	// Check if the error message contains "not found"
	return strings.Contains(err.Error(), "not found")
}

// WatchServices watches services for changes.
func (s *ServiceService) WatchServices(req *generated.WatchServicesRequest, stream generated.ServiceService_WatchServicesServer) error {
	s.logger.Debug("WatchServices called",
		log.Str("namespace", req.Namespace),
		log.Int("labelSelector", len(req.LabelSelector)),
		log.Int("fieldSelector", len(req.FieldSelector)))

	ctx := stream.Context()

	// Set default namespace if not specified
	namespace := req.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}

	// Start watching for service changes from orchestrator
	watchCh, err := s.orchestrator.WatchServices(ctx, namespace)
	if err != nil {
		s.logger.Error("Failed to watch services", log.Err(err))
		return status.Errorf(codes.Internal, "failed to watch services: %v", err)
	}

	// Initialize with current services (simulating ADDED events for all existing services)
	services, err := s.orchestrator.ListServices(ctx, namespace)
	if err != nil {
		s.logger.Error("Failed to list services", log.Err(err))
		return status.Errorf(codes.Internal, "failed to list initial services: %v", err)
	}

	// Send all existing services as ADDED events
	for _, service := range services {

		// Apply selector filtering
		if !matchSelectors(service, req.LabelSelector, req.FieldSelector) {
			continue
		}

		// Send to client
		err = stream.Send(&generated.WatchServicesResponse{
			Service:   client.ServiceToProto(service),
			EventType: generated.EventType_EVENT_TYPE_ADDED,
			Status:    &generated.Status{Code: int32(codes.OK)},
		})
		if err != nil {
			s.logger.Error("Failed to send initial service", log.Err(err))
			return status.Errorf(codes.Internal, "failed to send initial service: %v", err)
		}
	}

	// Watch loop - continue until client disconnects or context is cancelled
	for {
		select {
		case <-ctx.Done():
			s.logger.Debug("Watch context cancelled")
			return nil

		case event, ok := <-watchCh:
			if !ok {
				s.logger.Debug("Watch channel closed")
				return nil
			}

			// Only handle events for services
			if event.ResourceType != types.ResourceTypeService {
				continue
			}

			// Convert to typed service
			var service types.Service
			if typedService, ok := event.Resource.(*types.Service); ok {
				service = *typedService
			} else {
				// Use JSON marshaling/unmarshaling for conversion
				rawData, err := json.Marshal(event.Resource)
				if err != nil {
					s.logger.Warn("Failed to marshal service data", log.Err(err))
					continue
				}

				if err := json.Unmarshal(rawData, &service); err != nil {
					s.logger.Warn("Failed to unmarshal service data", log.Err(err))
					continue
				}
			}

			// Apply selector filtering
			if !matchSelectors(&service, req.LabelSelector, req.FieldSelector) {
				continue
			}

			// Map store event type to proto event type
			var eventType generated.EventType
			switch event.Type {
			case store.WatchEventCreated:
				eventType = generated.EventType_EVENT_TYPE_ADDED
			case store.WatchEventUpdated:
				eventType = generated.EventType_EVENT_TYPE_MODIFIED
			case store.WatchEventDeleted:
				eventType = generated.EventType_EVENT_TYPE_DELETED
			default:
				eventType = generated.EventType_EVENT_TYPE_UNSPECIFIED
			}

			// Send to client
			err = stream.Send(&generated.WatchServicesResponse{
				Service:   client.ServiceToProto(&service),
				EventType: eventType,
				Status:    &generated.Status{Code: int32(codes.OK)},
			})
			if err != nil {
				s.logger.Error("Failed to send watch event", log.Err(err))
				return status.Errorf(codes.Internal, "failed to send watch event: %v", err)
			}
		}
	}
}

// getServiceInstances retrieves and filters instances for a specific service
func (s *ServiceService) getServiceInstances(ctx context.Context, namespace, serviceID string) ([]*types.Instance, error) {
	// Get instances from orchestrator
	allInstances, err := s.orchestrator.ListInstances(ctx, namespace)
	if err != nil {
		return nil, err
	}

	// Filter instances that belong to this service and are not deleted
	var instances []*types.Instance
	for _, inst := range allInstances {
		if inst.ServiceID == serviceID && inst.Status != types.InstanceStatusDeleted {
			instances = append(instances, inst)
		}
	}
	return instances, nil
}

// countInstanceStatus counts running and pending instances
func (s *ServiceService) countInstanceStatus(instances []*types.Instance, instanceCache map[string]types.InstanceStatus) (running, pending int) {
	for _, inst := range instances {
		previousStatus, exists := instanceCache[inst.ID]

		// Only log status changes for debugging
		if !exists || previousStatus != inst.Status {
			instanceCache[inst.ID] = inst.Status
			s.logger.Debug("Instance status",
				log.Str("id", inst.ID),
				log.Str("status", string(inst.Status)))
		}

		switch inst.Status {
		case types.InstanceStatusRunning:
			running++
		case types.InstanceStatusPending, types.InstanceStatusStarting, types.InstanceStatusCreated:
			pending++
		}
	}
	return running, pending
}

// WatchScaling watches a service for scaling status changes and streams updates to the client.
func (s *ServiceService) WatchScaling(req *generated.WatchScalingRequest, stream generated.ServiceService_WatchScalingServer) error {
	s.logger.Debug("WatchScaling called",
		log.Str("service", req.ServiceName),
		log.Str("namespace", req.Namespace),
		log.Int("targetScale", int(req.TargetScale)))

	if req.ServiceName == "" {
		return status.Error(codes.InvalidArgument, "service name is required")
	}

	namespace := req.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}

	ctx := stream.Context()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Track consecutive completions for stability
	consecutiveCompletions := 0
	const requiredCompletions = 2

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			// Get current service state
			service, err := s.orchestrator.GetService(ctx, namespace, req.ServiceName)
			if err != nil {
				if IsNotFound(err) {
					return status.Errorf(codes.NotFound, "service not found: %s/%s", namespace, req.ServiceName)
				}
				s.logger.Error("Failed to get service for scaling watch", log.Err(err))
				return status.Errorf(codes.Internal, "failed to get service: %v", err)
			}

			// Get and count instances
			instances, err := s.getServiceInstances(ctx, namespace, service.ID)
			if err != nil {
				s.logger.Error("Failed to get service instances", log.Err(err))
				return status.Errorf(codes.Internal, "failed to get service instances: %v", err)
			}

			runningCount, pendingCount := s.countInstanceStatus(instances, make(map[string]types.InstanceStatus))

			// Determine if scaling is complete
			isComplete, targetScale := s.isScalingComplete(service, instances, runningCount, req.TargetScale)

			// Create and send response
			response := &generated.ScalingStatusResponse{
				CurrentScale:     utils.ToInt32NonNegative(service.Scale),
				TargetScale:      targetScale,
				RunningInstances: utils.ToInt32NonNegative(runningCount),
				PendingInstances: utils.ToInt32NonNegative(pendingCount),
				Complete:         isComplete,
				Status: &generated.Status{
					Code:    int32(codes.OK),
					Message: fmt.Sprintf("Service '%s' scaling progress", req.ServiceName),
				},
			}

			if err := stream.Send(response); err != nil {
				s.logger.Error("Failed to send scaling status", log.Err(err))
				return err
			}

			// Handle completion logic
			if isComplete {
				consecutiveCompletions++
				if consecutiveCompletions >= requiredCompletions {
					s.logger.Info("Scaling completed",
						log.Str("service", req.ServiceName),
						log.Str("namespace", namespace),
						log.Int("scale", int(targetScale)))
					return nil
				}
			} else {
				consecutiveCompletions = 0
			}
		}
	}
}

// isScalingComplete determines if scaling is complete based on service state and target
func (s *ServiceService) isScalingComplete(service *types.Service, instances []*types.Instance, runningCount int, targetScale int32) (bool, int32) {
	// For immediate scaling, the operation is completed immediately, so we just check if we've reached the target
	// Check if service scale and running instances match the target
	if targetScale == 0 {
		// Scale to zero: complete when no instances exist
		isComplete := len(instances) == 0
		s.logger.Debug("Scale to zero check", log.Int("instances", len(instances)), log.Bool("complete", isComplete))
		return isComplete, 0
	}

	// Scale up/down: complete when service scale and running instances match target
	isComplete := service.Scale == int(targetScale) && runningCount == int(targetScale)
	s.logger.Debug("Scale up/down check",
		log.Int("service_scale", service.Scale),
		log.Int("target_scale", int(targetScale)),
		log.Int("running_instances", runningCount),
		log.Bool("complete", isComplete))
	return isComplete, targetScale
}
