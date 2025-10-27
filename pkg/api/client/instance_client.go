package client

import (
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// InstanceClient provides methods for interacting with instances on the Rune API server.
type InstanceClient struct {
	client *Client
	logger log.Logger
	inst   generated.InstanceServiceClient
}

// NewInstanceClient creates a new instance client.
func NewInstanceClient(client *Client) *InstanceClient {
	return &InstanceClient{
		client: client,
		logger: client.logger.WithComponent("instance-client"),
		inst:   generated.NewInstanceServiceClient(client.conn),
	}
}

// GetInstance retrieves an instance by ID.
func (i *InstanceClient) GetInstance(namespace, id string) (*types.Instance, error) {
	i.logger.Debug("Getting instance", log.Str("id", id), log.Str("namespace", namespace))

	// Create the gRPC request
	req := &generated.GetInstanceRequest{
		Id:        id,
		Namespace: namespace,
	}

	// Send the request to the API server
	ctx, cancel := i.client.Context()
	defer cancel()

	resp, err := i.inst.GetInstance(ctx, req)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return nil, fmt.Errorf("instance not found: %s/%s", namespace, id)
		}
		i.logger.Error("Failed to get instance", log.Err(err), log.Str("id", id))
		return nil, convertGRPCError("get instance", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		i.logger.Error("Failed to get instance", log.Err(err), log.Str("id", id))
		return nil, err
	}

	// Convert the proto message to an instance
	instance, err := i.protoToInstance(resp.Instance)
	if err != nil {
		return nil, fmt.Errorf("failed to convert instance: %w", err)
	}

	return instance, nil
}

// ListInstances lists instances in a namespace with optional filtering.
func (i *InstanceClient) ListInstances(namespace, serviceID, labelSelector, fieldSelector string) ([]*types.Instance, error) {
	i.logger.Debug("Listing instances",
		log.Str("namespace", namespace),
		log.Str("serviceID", serviceID),
		log.Str("labelSelector", labelSelector),
		log.Str("fieldSelector", fieldSelector))

	// Create the gRPC request
	req := &generated.ListInstancesRequest{
		Namespace:     namespace,
		ServiceName:   serviceID,
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
	ctx, cancel := i.client.Context()
	defer cancel()

	resp, err := i.inst.ListInstances(ctx, req)
	if err != nil {
		i.logger.Error("Failed to list instances", log.Err(err), log.Str("namespace", namespace))
		return nil, convertGRPCError("list instances", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		i.logger.Error("Failed to list instances", log.Err(err), log.Str("namespace", namespace))
		return nil, err
	}

	// Convert the proto messages to instances
	instances := make([]*types.Instance, 0, len(resp.Instances))
	for _, protoInstance := range resp.Instances {
		instance, err := i.protoToInstance(protoInstance)
		if err != nil {
			i.logger.Error("Failed to convert instance", log.Err(err))
			continue
		}
		instances = append(instances, instance)
	}

	return instances, nil
}

// StartInstance starts an instance.
func (i *InstanceClient) StartInstance(namespace, id string) error {
	i.logger.Debug("Starting instance", log.Str("id", id), log.Str("namespace", namespace))

	// Create the gRPC request
	req := &generated.InstanceActionRequest{
		Id: id,
	}

	// Send the request to the API server
	ctx, cancel := i.client.Context()
	defer cancel()

	resp, err := i.inst.StartInstance(ctx, req)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return fmt.Errorf("instance not found: %s/%s", namespace, id)
		}
		i.logger.Error("Failed to start instance", log.Err(err), log.Str("id", id))
		return convertGRPCError("start instance", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		i.logger.Error("Failed to start instance", log.Err(err), log.Str("id", id))
		return err
	}

	return nil
}

// StopInstance stops an instance.
func (i *InstanceClient) StopInstance(namespace, id string, timeout time.Duration) error {
	i.logger.Debug("Stopping instance",
		log.Str("id", id),
		log.Str("namespace", namespace),
		log.Str("timeout", timeout.String()))

	// Create the gRPC request
	req := &generated.InstanceActionRequest{
		Id:             id,
		TimeoutSeconds: int32(timeout.Seconds()),
	}

	// Send the request to the API server
	ctx, cancel := i.client.Context()
	defer cancel()

	resp, err := i.inst.StopInstance(ctx, req)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return fmt.Errorf("instance not found: %s/%s", namespace, id)
		}
		i.logger.Error("Failed to stop instance", log.Err(err), log.Str("id", id))
		return convertGRPCError("stop instance", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		i.logger.Error("Failed to stop instance", log.Err(err), log.Str("id", id))
		return err
	}

	return nil
}

// RestartInstance restarts an instance.
func (i *InstanceClient) RestartInstance(namespace, id string, timeout time.Duration) error {
	i.logger.Debug("Restarting instance",
		log.Str("id", id),
		log.Str("namespace", namespace),
		log.Str("timeout", timeout.String()))

	// Create the gRPC request
	req := &generated.InstanceActionRequest{
		Id:             id,
		TimeoutSeconds: int32(timeout.Seconds()),
	}

	// Send the request to the API server
	ctx, cancel := i.client.Context()
	defer cancel()

	resp, err := i.inst.RestartInstance(ctx, req)
	if err != nil {
		statusErr, ok := status.FromError(err)
		if ok && statusErr.Code() == codes.NotFound {
			return fmt.Errorf("instance not found: %s/%s", namespace, id)
		}
		i.logger.Error("Failed to restart instance", log.Err(err), log.Str("id", id))
		return convertGRPCError("restart instance", err)
	}

	// Check if the API returned an error status
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		err := fmt.Errorf("API error: %s", resp.Status.Message)
		i.logger.Error("Failed to restart instance", log.Err(err), log.Str("id", id))
		return err
	}

	return nil
}

// InstanceWatchEvent represents an instance change event
type InstanceWatchEvent struct {
	Instance  *types.Instance
	EventType string // "ADDED", "MODIFIED", "DELETED"
	Error     error
}

// WatchInstances watches instances for changes and returns a channel of events.
// Note: This simulates watch by polling the list endpoint, since we need to regenerate
// the protobuf code to include the new WatchInstances API.
func (i *InstanceClient) WatchInstances(namespace, serviceID, labelSelector, fieldSelector string) (<-chan InstanceWatchEvent, error) {
	i.logger.Debug("Watching instances",
		log.Str("namespace", namespace),
		log.Str("serviceID", serviceID),
		log.Str("labelSelector", labelSelector),
		log.Str("fieldSelector", fieldSelector))

	// Create a channel for watch events
	eventCh := make(chan InstanceWatchEvent)

	// Create a context with the client timeout - we'll use this for the initial list
	ctx, cancel := i.client.Context()
	defer cancel()

	// Do an initial list to get the current state
	instances, err := i.ListInstances(namespace, serviceID, labelSelector, fieldSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to perform initial list for watch: %w", err)
	}

	// Start a goroutine to poll for changes and send them to the channel
	go func() {
		defer close(eventCh)

		// Keep track of instances we've seen
		knownInstances := make(map[string]*types.Instance)
		for _, instance := range instances {
			// Send initial ADDED events for all instances
			eventCh <- InstanceWatchEvent{
				Instance:  instance,
				EventType: "ADDED",
				Error:     nil,
			}
			knownInstances[instance.ID] = instance
		}

		// Poll every 2 seconds
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// Create a new context for each poll
				_, pollCancel := i.client.Context()
				defer pollCancel()

				// List instances again
				currentInstances, err := i.ListInstances(namespace, serviceID, labelSelector, fieldSelector)
				if err != nil {
					// Send error event and continue
					eventCh <- InstanceWatchEvent{
						Error: fmt.Errorf("error during watch poll: %w", err),
					}
					continue
				}

				// Track instances we've seen in this poll
				seenInstances := make(map[string]bool)

				// Look for new or modified instances
				for _, instance := range currentInstances {
					seenInstances[instance.ID] = true

					if prev, exists := knownInstances[instance.ID]; exists {
						// Check if the instance has changed
						if !instanceEquals(prev, instance) {
							// Send MODIFIED event
							eventCh <- InstanceWatchEvent{
								Instance:  instance,
								EventType: "MODIFIED",
								Error:     nil,
							}
							knownInstances[instance.ID] = instance
						}
					} else {
						// New instance, send ADDED event
						eventCh <- InstanceWatchEvent{
							Instance:  instance,
							EventType: "ADDED",
							Error:     nil,
						}
						knownInstances[instance.ID] = instance
					}
				}

				// Look for deleted instances
				for id, instance := range knownInstances {
					if _, exists := seenInstances[id]; !exists {
						// Instance was deleted, send DELETED event
						eventCh <- InstanceWatchEvent{
							Instance:  instance,
							EventType: "DELETED",
							Error:     nil,
						}
						delete(knownInstances, id)
					}
				}

			case <-ctx.Done():
				// Client context canceled
				i.logger.Debug("Watch context cancelled")
				return
			}
		}
	}()

	return eventCh, nil
}

// instanceEquals compares two instances to see if they're functionally equivalent
func instanceEquals(a, b *types.Instance) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.ID == b.ID &&
		a.Name == b.Name &&
		a.Namespace == b.Namespace &&
		a.ServiceID == b.ServiceID &&
		a.NodeID == b.NodeID &&
		a.Status == b.Status &&
		a.IP == b.IP
}

// Helper function to convert a protobuf Instance message to a types.Instance
func (i *InstanceClient) protoToInstance(proto *generated.Instance) (*types.Instance, error) {
	if proto == nil {
		return nil, fmt.Errorf("proto instance is nil")
	}

	// Create a new Instance with basic fields
	instance := &types.Instance{
		ID:            proto.Id,
		Runner:        types.RunnerType(proto.Runner),
		Name:          proto.Name,
		Namespace:     proto.Namespace,
		ServiceID:     proto.ServiceId,
		ServiceName:   proto.ServiceName,
		NodeID:        proto.NodeId,
		IP:            proto.Ip,
		StatusMessage: proto.StatusMessage,
		ContainerID:   proto.ContainerId,
		PID:           int(proto.Pid),
		Environment:   proto.Environment,
		Metadata: &types.InstanceMetadata{
			ServiceGeneration: int64(proto.Metadata.Generation),
			RestartCount:      int(proto.Metadata.RestartCount),
			DeletionTimestamp: utils.ProtoStringToTimestamp(proto.Metadata.DeletionTimestamp),
		},
	}

	if proto.Resources != nil {
		instance.Resources = &types.Resources{}
		if proto.Resources.Cpu != nil {
			instance.Resources.CPU = types.ResourceLimit{
				Request: proto.Resources.Cpu.Request,
				Limit:   proto.Resources.Cpu.Limit,
			}
		}

		if proto.Resources.Memory != nil {
			instance.Resources.Memory = types.ResourceLimit{
				Request: proto.Resources.Memory.Request,
				Limit:   proto.Resources.Memory.Limit,
			}
		}
	}

	// Parse timestamps
	createdAt, err := parseTimestamp(proto.CreatedAt)
	if err != nil {
		i.logger.Warn("Failed to parse created_at timestamp",
			log.Str("instance", proto.Id),
			log.Str("timestamp", proto.CreatedAt),
			log.Err(err))
	} else {
		instance.CreatedAt = *createdAt
	}

	updatedAt, err := parseTimestamp(proto.UpdatedAt)
	if err != nil {
		i.logger.Warn("Failed to parse updated_at timestamp",
			log.Str("instance", proto.Id),
			log.Str("timestamp", proto.UpdatedAt),
			log.Err(err))
	} else {
		instance.UpdatedAt = *updatedAt
	}

	deletionTimestamp, err := parseTimestamp(proto.Metadata.DeletionTimestamp)
	if err != nil {
		i.logger.Warn("Failed to parse deletion timestamp",
			log.Str("instance", proto.Id),
			log.Str("timestamp", proto.Metadata.DeletionTimestamp),
			log.Err(err))
	} else {
		instance.Metadata.DeletionTimestamp = deletionTimestamp
	}

	// Convert status
	switch proto.Status {
	case generated.InstanceStatus_INSTANCE_STATUS_PENDING:
		instance.Status = types.InstanceStatusPending
	case generated.InstanceStatus_INSTANCE_STATUS_CREATED:
		instance.Status = types.InstanceStatusCreated
	case generated.InstanceStatus_INSTANCE_STATUS_STARTING:
		instance.Status = types.InstanceStatusStarting
	case generated.InstanceStatus_INSTANCE_STATUS_RUNNING:
		instance.Status = types.InstanceStatusRunning
	case generated.InstanceStatus_INSTANCE_STATUS_STOPPED:
		instance.Status = types.InstanceStatusStopped
	case generated.InstanceStatus_INSTANCE_STATUS_FAILED:
		instance.Status = types.InstanceStatusFailed
	case generated.InstanceStatus_INSTANCE_STATUS_EXITED:
		instance.Status = types.InstanceStatusExited
	case generated.InstanceStatus_INSTANCE_STATUS_DELETED:
		instance.Status = types.InstanceStatusDeleted
	default:
		instance.Status = types.InstanceStatusPending // Default to pending if unspecified
	}

	// Convert resources
	if proto.Resources != nil {
		instance.Resources = &types.Resources{}

		if proto.Resources.Cpu != nil {
			instance.Resources.CPU = types.ResourceLimit{
				Request: proto.Resources.Cpu.Request,
				Limit:   proto.Resources.Cpu.Limit,
			}
		}

		if proto.Resources.Memory != nil {
			instance.Resources.Memory = types.ResourceLimit{
				Request: proto.Resources.Memory.Request,
				Limit:   proto.Resources.Memory.Limit,
			}
		}
	}

	return instance, nil
}
