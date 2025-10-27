package service

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// InstanceService implements the gRPC InstanceService.
type InstanceService struct {
	generated.UnimplementedInstanceServiceServer

	store         store.Store
	runnerManager manager.IRunnerManager
	logger        log.Logger
}

// NewInstanceService creates a new InstanceService with the given store, runners, and logger.
func NewInstanceService(store store.Store, runnerManager manager.IRunnerManager, logger log.Logger) *InstanceService {
	return &InstanceService{
		store:         store,
		runnerManager: runnerManager,
		logger:        logger.WithComponent("instance-service"),
	}
}

// GetInstance retrieves an instance by ID.
func (s *InstanceService) GetInstance(ctx context.Context, req *generated.GetInstanceRequest) (*generated.InstanceResponse, error) {
	s.logger.Debug("GetInstance called", log.Str("id", req.Id))

	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "instance ID is required")
	}

	if req.Namespace == "" {
		req.Namespace = DefaultNamespace
	}

	instance, err := s.store.GetInstanceByID(ctx, req.Namespace, req.Id)
	if err != nil {
		// Handle error case
		return nil, status.Errorf(codes.Internal, "failed to get instance: %v", err)
	}

	// Get instance status from the appropriate runner
	var status types.InstanceStatus
	var statusErr error

	runnerToUse, err := s.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return nil, err
	}

	status, statusErr = runnerToUse.Status(ctx, instance)

	if statusErr != nil {
		s.logger.Warn("Failed to get instance status", log.Str("id", req.Id), log.Err(statusErr))
		// We'll continue with the stored status, just log the error
	} else {
		// Update the instance status in our response
		instance.Status = status
	}

	// Convert to protobuf message
	protoInstance, err := s.instanceModelToProto(instance)
	if err != nil {
		return nil, fmt.Errorf("failed to convert instance to proto: %v", err)
	}

	return &generated.InstanceResponse{
		Instance: protoInstance,
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: "Instance retrieved successfully",
		},
	}, nil
}

// ListInstances lists instances with optional filtering.
func (s *InstanceService) ListInstances(ctx context.Context, req *generated.ListInstancesRequest) (*generated.ListInstancesResponse, error) {
	s.logger.Debug("ListInstances called")

	namespace := req.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}

	// Get instances from the store
	var storeInstances []types.Instance
	err := s.store.List(ctx, types.ResourceTypeInstance, namespace, &storeInstances)
	if err != nil {
		s.logger.Error("Failed to list instances", log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to list instances: %v", err)
	}

	// Convert to domain model instances
	instances := make([]*types.Instance, 0, len(storeInstances))
	for _, instance := range storeInstances {

		// Apply service name filter if provided
		if req.ServiceName != "" && instance.ServiceID != req.ServiceName {
			continue
		}

		// Apply node ID filter if provided
		if req.NodeId != "" && instance.NodeID != req.NodeId {
			continue
		}

		// Apply status filter if provided
		if req.Status != generated.InstanceStatus_INSTANCE_STATUS_UNSPECIFIED {
			protoStatus := s.instanceStatusToProto(instance.Status)
			if protoStatus != req.Status {
				continue
			}
		}

		instances = append(instances, &instance)
	}

	// Convert to protobuf messages
	protoInstances := make([]*generated.Instance, 0, len(instances))
	for _, instance := range instances {
		// Get instance status from the appropriate runner
		var status types.InstanceStatus
		var statusErr error

		runnerToUse, err := s.runnerManager.GetInstanceRunner(instance)
		if err != nil {
			return nil, err
		}
		status, statusErr = runnerToUse.Status(ctx, instance)

		if statusErr == nil {
			// Update the instance status
			instance.Status = status
		}

		protoInstance, err := s.instanceModelToProto(instance)
		if err != nil {
			s.logger.Error("Failed to convert instance to proto", log.Err(err))
			continue
		}

		protoInstances = append(protoInstances, protoInstance)
	}

	return &generated.ListInstancesResponse{
		Instances: protoInstances,
		Status: &generated.Status{
			Code:    int32(codes.OK),
			Message: fmt.Sprintf("Found %d instances", len(protoInstances)),
		},
		Paging: &generated.PagingParams{
			Limit:  utils.ToInt32NonNegative(len(protoInstances)),
			Offset: 0,
		},
	}, nil
}

// StartInstance starts an instance.
func (s *InstanceService) StartInstance(ctx context.Context, req *generated.InstanceActionRequest) (*generated.InstanceResponse, error) {
	s.logger.Debug("StartInstance called", log.Str("id", req.Id))

	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "instance ID is required")
	}

	// Get the instance
	instanceResp, err := s.GetInstance(ctx, &generated.GetInstanceRequest{Id: req.Id})
	if err != nil {
		return nil, err
	}

	// Determine the appropriate runner
	instance, err := s.ProtoInstanceToInstanceModel(instanceResp.Instance)
	if err != nil {
		return nil, err
	}
	runnerToUse, err := s.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return nil, err
	}

	// Start the instance
	if err := runnerToUse.Start(ctx, instance); err != nil {
		s.logger.Error("Failed to start instance", log.Str("id", req.Id), log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to start instance: %v", err)
	}

	// Get the updated instance
	return s.GetInstance(ctx, &generated.GetInstanceRequest{Id: req.Id})
}

// StopInstance stops an instance.
func (s *InstanceService) StopInstance(ctx context.Context, req *generated.InstanceActionRequest) (*generated.InstanceResponse, error) {
	s.logger.Debug("StopInstance called", log.Str("id", req.Id))

	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "instance ID is required")
	}

	// Get the instance
	instanceResp, err := s.GetInstance(ctx, &generated.GetInstanceRequest{Id: req.Id})
	if err != nil {
		return nil, err
	}

	// Determine the appropriate runner
	instance, err := s.ProtoInstanceToInstanceModel(instanceResp.Instance)
	if err != nil {
		return nil, err
	}
	runnerToUse, err := s.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return nil, err
	}

	// Set timeout
	timeout := time.Duration(30) * time.Second
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}

	// Stop the instance
	if err := runnerToUse.Stop(ctx, instance, timeout); err != nil {
		s.logger.Error("Failed to stop instance", log.Str("id", req.Id), log.Err(err))
		return nil, status.Errorf(codes.Internal, "failed to stop instance: %v", err)
	}

	// Get the updated instance
	return s.GetInstance(ctx, &generated.GetInstanceRequest{Id: req.Id})
}

// RestartInstance restarts an instance.
func (s *InstanceService) RestartInstance(ctx context.Context, req *generated.InstanceActionRequest) (*generated.InstanceResponse, error) {
	s.logger.Debug("RestartInstance called", log.Str("id", req.Id))

	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "instance ID is required")
	}

	// First stop the instance
	_, err := s.StopInstance(ctx, req)
	if err != nil {
		return nil, err
	}

	// Then start it again
	return s.StartInstance(ctx, req)
}

// ProtoInstanceToInstanceModel converts a protobuf message to a domain model instance.
func (s *InstanceService) ProtoInstanceToInstanceModel(protoInstance *generated.Instance) (*types.Instance, error) {
	if protoInstance == nil {
		return nil, fmt.Errorf("proto instance is nil")
	}

	// Create a new Instance with basic fields
	instance := &types.Instance{
		ID:            protoInstance.Id,
		Name:          protoInstance.Name,
		Namespace:     protoInstance.Namespace,
		ServiceID:     protoInstance.ServiceId,
		ServiceName:   protoInstance.ServiceName,
		NodeID:        protoInstance.NodeId,
		IP:            protoInstance.Ip,
		StatusMessage: protoInstance.StatusMessage,
		ContainerID:   protoInstance.ContainerId,
		Environment:   protoInstance.Environment,
		Metadata: &types.InstanceMetadata{
			ServiceGeneration: int64(protoInstance.Metadata.Generation),
			RestartCount:      int(protoInstance.Metadata.RestartCount),
			DeletionTimestamp: utils.ProtoStringToTimestamp(protoInstance.Metadata.DeletionTimestamp),
		},
	}

	// Convert PID (int32 -> int)
	if protoInstance.Pid > 0 {
		instance.PID = int(protoInstance.Pid)
	}

	// Parse timestamps
	if protoInstance.CreatedAt != "" {
		createdAt, err := time.Parse(time.RFC3339, protoInstance.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("invalid created_at timestamp: %w", err)
		}
		instance.CreatedAt = createdAt
	} else {
		instance.CreatedAt = time.Now() // Default to current time if not provided
	}

	if protoInstance.UpdatedAt != "" {
		updatedAt, err := time.Parse(time.RFC3339, protoInstance.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("invalid updated_at timestamp: %w", err)
		}
		instance.UpdatedAt = updatedAt
	} else {
		instance.UpdatedAt = instance.CreatedAt // Default to created time if not provided
	}

	// Convert status
	instance.Status = s.protoStatusToInstanceStatus(protoInstance.Status)

	// Convert resources if provided
	if protoInstance.Resources != nil {
		instance.Resources = &types.Resources{}

		if protoInstance.Resources.Cpu != nil {
			instance.Resources.CPU = types.ResourceLimit{
				Request: protoInstance.Resources.Cpu.Request,
				Limit:   protoInstance.Resources.Cpu.Limit,
			}
		}

		if protoInstance.Resources.Memory != nil {
			instance.Resources.Memory = types.ResourceLimit{
				Request: protoInstance.Resources.Memory.Request,
				Limit:   protoInstance.Resources.Memory.Limit,
			}
		}
	}

	// Validate the instance
	if err := instance.Validate(); err != nil {
		return nil, fmt.Errorf("invalid instance: %w", err)
	}

	return instance, nil
}

// Convert proto status enum to domain model status
func (s *InstanceService) protoStatusToInstanceStatus(status generated.InstanceStatus) types.InstanceStatus {
	switch status {
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
	default:
		return types.InstanceStatusPending // Default to pending if unspecified
	}
}

// instanceModelToProto converts a domain model instance to a protobuf message.
func (s *InstanceService) instanceModelToProto(instance *types.Instance) (*generated.Instance, error) {
	if instance == nil {
		return nil, fmt.Errorf("instance is nil")
	}

	protoInstance := &generated.Instance{
		Id:            instance.ID,
		Runner:        string(instance.Runner),
		Namespace:     instance.Namespace,
		Name:          instance.Name,
		ServiceId:     instance.ServiceID,
		ServiceName:   instance.ServiceName,
		NodeId:        instance.NodeID,
		Ip:            instance.IP,
		StatusMessage: instance.StatusMessage,
		ContainerId:   instance.ContainerID,
		Pid:           utils.ToInt32NonNegative(instance.PID),
		Environment:   instance.Environment,
		Metadata: &generated.InstanceMetadata{
			Generation:   utils.ToInt32NonNegative64(instance.Metadata.ServiceGeneration),
			RestartCount: utils.ToInt32NonNegative(instance.Metadata.RestartCount),
		},
		CreatedAt: instance.CreatedAt.Format(time.RFC3339),
		UpdatedAt: instance.UpdatedAt.Format(time.RFC3339),
	}

	if instance.Metadata.DeletionTimestamp != nil {
		protoInstance.Metadata.DeletionTimestamp = instance.Metadata.DeletionTimestamp.Format(time.RFC3339)
	}

	// Convert status
	protoInstance.Status = s.instanceStatusToProto(instance.Status)

	// Convert resources
	if instance.Resources != nil {
		protoInstance.Resources = &generated.Resources{
			Cpu: &generated.ResourceLimit{
				Request: instance.Resources.CPU.Request,
				Limit:   instance.Resources.CPU.Limit,
			},
			Memory: &generated.ResourceLimit{
				Request: instance.Resources.Memory.Request,
				Limit:   instance.Resources.Memory.Limit,
			},
		}
	}

	return protoInstance, nil
}

// instanceStatusToProto converts a domain model instance status to a protobuf enum value.
func (s *InstanceService) instanceStatusToProto(status types.InstanceStatus) generated.InstanceStatus {
	switch status {
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
		return generated.InstanceStatus_INSTANCE_STATUS_UNSPECIFIED
	}
}
