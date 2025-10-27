package controllers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// ScalingController manages scaling operations for services
type ScalingController interface {
	// Start the scaling controller
	Start(ctx context.Context) error

	// Stop the scaling controller
	Stop()

	// CreateScalingOperation creates a new scaling operation
	CreateScalingOperation(ctx context.Context, service *types.Service, params types.ScalingOperationParams) error

	// GetActiveOperation gets the active scaling operation for a service
	GetActiveOperation(ctx context.Context, namespace, serviceName string) (*types.ScalingOperation, error)
}

// ScalingController manages scaling operations for services
type scalingController struct {
	store            store.Store
	logger           log.Logger
	ops              sync.Map // Track active scaling operations
	wg               sync.WaitGroup
	ctx              context.Context
	cancel           context.CancelFunc
	operationWatchCh <-chan store.WatchEvent
	internalUpdates  sync.Map
}

// NewScalingController creates a new scaling controller
func NewScalingController(store store.Store, logger log.Logger) ScalingController {
	return &scalingController{
		store:  store,
		logger: logger.WithComponent("scaling-controller"),
	}
}

// Start the scaling controller
func (c *scalingController) Start(ctx context.Context) error {
	c.logger.Info("Starting scaling controller")

	// Create context with cancel for all operations
	c.ctx, c.cancel = context.WithCancel(ctx)

	// Start watching scaling operations
	operationWatchCh, err := c.store.Watch(c.ctx, types.ResourceTypeScalingOperation, "")
	if err != nil {
		return fmt.Errorf("failed to watch scaling operations: %w", err)
	}
	c.operationWatchCh = operationWatchCh

	// Start a goroutine to process scaling operation events
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.watchScalingOperations()
	}()

	// Recover any in-progress scaling operations that might have been interrupted by a server restart
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		if err := c.recoverInProgressOperations(ctx); err != nil {
			c.logger.Error("Error recovering in-progress scaling operations", log.Err(err))
		}

		// After recovering operations, check if any are at target scale and should be completed
		c.checkOperationsAtTargetScale(ctx)
	}()

	return nil
}

// Stop the scaling controller
func (c *scalingController) Stop() {
	c.logger.Info("Stopping scaling controller")

	// Cancel context to stop all operations
	if c.cancel != nil {
		c.cancel()
	}

	// Wait for all goroutines to finish
	c.wg.Wait()
}

// Watch scaling operations
func (c *scalingController) watchScalingOperations() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case event, ok := <-c.operationWatchCh:
			if !ok {
				c.logger.Error("Scaling operation watch channel closed, restarting watch")
				// Try to restart the watch
				watchCh, err := c.store.Watch(c.ctx, types.ResourceTypeScalingOperation, "")
				if err != nil {
					c.logger.Error("Failed to restart scaling operation watch", log.Err(err))
					time.Sleep(5 * time.Second) // Backoff before retry
					continue
				}
				c.operationWatchCh = watchCh
				continue
			}

			// Skip internal updates
			if c.isInternalUpdate(event) {
				continue
			}

			// Only handle created and updated events
			if event.Type != store.WatchEventCreated && event.Type != store.WatchEventUpdated {
				continue
			}

			// Process the scaling operation event
			go c.processScalingOperationEvent(c.ctx, event)
		}
	}
}

// Process a scaling operation event
func (c *scalingController) processScalingOperationEvent(ctx context.Context, event store.WatchEvent) {
	scalingOp, ok := event.Resource.(*types.ScalingOperation)
	if !ok {
		c.logger.Error("Failed to convert event resource to ScalingOperation",
			log.Any("event", event))
		return
	}

	// For newly created operations, start the scaling process
	if event.Type == store.WatchEventCreated && scalingOp.Status == types.ScalingOperationStatusInProgress {
		c.logger.Info("Processing new scaling operation",
			log.Str("service", scalingOp.ServiceName),
			log.Str("operation_id", scalingOp.ID),
			log.Int("current", scalingOp.CurrentScale),
			log.Int("target", scalingOp.TargetScale))

		// For gradual scaling, start a goroutine
		if scalingOp.Mode == types.ScalingModeGradual {
			if err := c.startGradualScaling(ctx, scalingOp); err != nil {
				c.logger.Error("Failed to start gradual scaling",
					log.Str("service", scalingOp.ServiceName),
					log.Str("operation_id", scalingOp.ID),
					log.Err(err))
			}
		} else {
			// For immediate scaling, update the service directly
			if err := c.executeImmediateScaling(ctx, scalingOp); err != nil {
				c.logger.Error("Failed to execute immediate scaling",
					log.Str("service", scalingOp.ServiceName),
					log.Str("operation_id", scalingOp.ID),
					log.Err(err))
			}
		}
	}
}

// Check if an event was triggered internally
func (c *scalingController) isInternalUpdate(event store.WatchEvent) bool {
	// Check if event has an orchestrator source
	if event.Source == "orchestrator" {
		return true
	}

	// Check if we have a record of this being an internal update
	key := fmt.Sprintf("%s/%s/%s", event.ResourceType, event.Namespace, event.Name)
	_, exists := c.internalUpdates.Load(key)
	return exists
}

// Mark an update as being in progress
func (c *scalingController) inProgressUpdate(resourceType types.ResourceType, namespace, name string) {
	key := fmt.Sprintf("%s/%s/%s", resourceType, namespace, name)
	c.internalUpdates.Store(key, time.Now())
}

// Clear an update marked as in progress
func (c *scalingController) clearInProgressUpdate(resourceType types.ResourceType, namespace, name string) {
	key := fmt.Sprintf("%s/%s/%s", resourceType, namespace, name)
	c.internalUpdates.Delete(key)
}

// CreateScalingOperation creates a new scaling operation for a service
func (c *scalingController) CreateScalingOperation(ctx context.Context, service *types.Service, params types.ScalingOperationParams) error {
	// Create scaling operation
	op := &types.ScalingOperation{
		ID:           fmt.Sprintf("scale-%s-%d", service.Name, time.Now().UnixNano()),
		Namespace:    service.Namespace,
		ServiceName:  service.Name,
		CurrentScale: params.CurrentScale,
		TargetScale:  params.TargetScale,
		StepSize:     params.StepSize,
		Interval:     params.IntervalSeconds,
		StartTime:    time.Now(),
		Status:       types.ScalingOperationStatusInProgress,
		Mode:         types.ScalingModeImmediate,
	}

	if params.IsGradual {
		op.Mode = types.ScalingModeGradual
	}

	c.logger.Info("Creating scaling operation",
		log.Str("service", service.Name),
		log.Int("from", params.CurrentScale),
		log.Int("to", params.TargetScale),
		log.Bool("gradual", params.IsGradual))

	// Store the operation
	if err := c.store.Create(ctx, types.ResourceTypeScalingOperation, service.Namespace, op.ID, op); err != nil {
		return fmt.Errorf("failed to store scaling operation: %w", err)
	}

	return nil
}

// Execute immediate scaling by updating the service directly
func (c *scalingController) executeImmediateScaling(ctx context.Context, op *types.ScalingOperation) error {
	// Get the service
	var service types.Service
	if err := c.store.Get(ctx, types.ResourceTypeService, op.Namespace, op.ServiceName, &service); err != nil {
		c.failOperation(ctx, op, fmt.Sprintf("failed to get service: %v", err))
		return err
	}

	// Mark as in-progress to prevent circular updates
	c.inProgressUpdate(types.ResourceTypeService, service.Namespace, service.Name)
	defer c.clearInProgressUpdate(types.ResourceTypeService, service.Namespace, service.Name)

	// Update service scale directly
	service.Scale = op.TargetScale
	service.Metadata.UpdatedAt = time.Now()

	if err := c.store.Update(ctx, types.ResourceTypeService, service.Namespace, service.Name, &service); err != nil {
		c.failOperation(ctx, op, fmt.Sprintf("failed to update service scale: %v", err))
		return err
	}

	// Mark operation as completed
	return c.completeOperation(ctx, op)
}

// startGradualScaling starts a goroutine to handle gradual scaling
func (c *scalingController) startGradualScaling(ctx context.Context, op *types.ScalingOperation) error {
	opKey := fmt.Sprintf("%s/%s", op.Namespace, op.ServiceName)

	// Check if operation already exists
	if _, exists := c.ops.Load(opKey); exists {
		return fmt.Errorf("scaling operation already in progress for service %s", op.ServiceName)
	}

	// Create operation context
	opCtx, opCancel := context.WithCancel(ctx)
	c.ops.Store(opKey, opCancel)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.ops.Delete(opKey)
		defer opCancel()

		// Immediately process the first step
		c.logger.Info("Processing initial scaling step immediately after operation start/recovery",
			log.Str("operation_id", op.ID),
			log.Str("service", op.ServiceName))

		if err := c.processScalingStep(ctx, op); err != nil {
			c.logger.Error("Failed to process initial scaling step",
				log.Str("service", op.ServiceName),
				log.Err(err))
			return
		}

		// Check if operation completed in the first step
		var currentOp types.ScalingOperation
		if err := c.store.Get(ctx, types.ResourceTypeScalingOperation, op.Namespace, op.ID, &currentOp); err != nil {
			c.logger.Error("Failed to get scaling operation status after initial step",
				log.Str("service", op.ServiceName),
				log.Err(err))
			return
		}

		if currentOp.Status != types.ScalingOperationStatusInProgress {
			c.logger.Info("Operation completed in initial step, exiting gradient scaling goroutine",
				log.Str("operation_id", op.ID),
				log.Str("service", op.ServiceName))
			return
		}

		ticker := time.NewTicker(time.Duration(op.Interval) * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-opCtx.Done():
				return
			case <-ticker.C:
				if err := c.processScalingStep(ctx, op); err != nil {
					c.logger.Error("Failed to process scaling step",
						log.Str("service", op.ServiceName),
						log.Err(err))
					return
				}

				// Check if operation is completed
				var currentOp types.ScalingOperation
				if err := c.store.Get(ctx, types.ResourceTypeScalingOperation, op.Namespace, op.ID, &currentOp); err != nil {
					c.logger.Error("Failed to get scaling operation status",
						log.Str("service", op.ServiceName),
						log.Err(err))
					return
				}

				if currentOp.Status != types.ScalingOperationStatusInProgress {
					c.logger.Info("Operation completed, exiting gradient scaling goroutine",
						log.Str("operation_id", op.ID),
						log.Str("service", op.ServiceName))
					return
				}
			}
		}
	}()

	return nil
}

// processScalingStep processes a single step of gradual scaling
func (c *scalingController) processScalingStep(ctx context.Context, op *types.ScalingOperation) error {
	// Get fresh copy of operation
	var currentOp types.ScalingOperation
	if err := c.store.Get(ctx, types.ResourceTypeScalingOperation, op.Namespace, op.ID, &currentOp); err != nil {
		return fmt.Errorf("failed to get scaling operation: %w", err)
	}

	if currentOp.Status != types.ScalingOperationStatusInProgress {
		c.logger.Debug("Scaling operation no longer in progress, skipping step",
			log.Str("operation_id", op.ID),
			log.Str("status", string(currentOp.Status)))
		return nil
	}

	// Get service
	var service types.Service
	if err := c.store.Get(ctx, types.ResourceTypeService, op.Namespace, op.ServiceName, &service); err != nil {
		return fmt.Errorf("failed to get service: %w", err)
	}

	c.logger.Debug("Processing scaling step",
		log.Str("operation_id", op.ID),
		log.Str("service", op.ServiceName),
		log.Int("current_scale", service.Scale),
		log.Int("target_scale", op.TargetScale))

	// Update the operation with actual current scale
	if currentOp.CurrentScale != service.Scale {
		currentOp.CurrentScale = service.Scale
		if err := c.store.Update(ctx, types.ResourceTypeScalingOperation, op.Namespace, op.ID, &currentOp); err != nil {
			c.logger.Error("Failed to update operation with current scale",
				log.Str("operation_id", op.ID),
				log.Err(err))
		}
	}

	// If we've already reached the target scale, complete the operation
	if service.Scale == op.TargetScale {
		c.logger.Info("Target scale already reached, completing operation",
			log.Str("operation_id", op.ID),
			log.Str("service", op.ServiceName),
			log.Int("scale", service.Scale))
		return c.completeOperation(ctx, &currentOp)
	}

	// Calculate next scale
	var nextScale int
	if op.TargetScale > service.Scale {
		nextScale = min(service.Scale+op.StepSize, op.TargetScale)
	} else {
		nextScale = max(service.Scale-op.StepSize, op.TargetScale)
	}

	// Update service if scale changed
	if service.Scale != nextScale {
		c.inProgressUpdate(types.ResourceTypeService, service.Namespace, service.Name)
		defer c.clearInProgressUpdate(types.ResourceTypeService, service.Namespace, service.Name)

		c.logger.Info("Updating service scale",
			log.Str("operation_id", op.ID),
			log.Str("service", op.ServiceName),
			log.Int("from", service.Scale),
			log.Int("to", nextScale))

		service.Scale = nextScale
		service.Metadata.UpdatedAt = time.Now()
		if err := c.store.Update(ctx, types.ResourceTypeService, op.Namespace, op.ServiceName, &service); err != nil {
			return fmt.Errorf("failed to update service scale: %w", err)
		}
	}

	// Check if we've reached the target
	if nextScale == op.TargetScale {
		c.logger.Info("Target scale reached, completing operation",
			log.Str("operation_id", op.ID),
			log.Str("service", op.ServiceName),
			log.Int("scale", nextScale))
		return c.completeOperation(ctx, &currentOp)
	}

	return nil
}

// completeOperation marks a scaling operation as completed
func (c *scalingController) completeOperation(ctx context.Context, op *types.ScalingOperation) error {
	c.logger.Info("Completing scaling operation",
		log.Str("operation_id", op.ID),
		log.Str("service", op.ServiceName),
		log.Int("from", op.CurrentScale),
		log.Int("to", op.TargetScale))

	op.Status = types.ScalingOperationStatusCompleted
	op.EndTime = time.Now()
	return c.store.Update(ctx, types.ResourceTypeScalingOperation, op.Namespace, op.ID, op)
}

// failOperation marks a scaling operation as failed
func (c *scalingController) failOperation(ctx context.Context, op *types.ScalingOperation, reason string) error {
	op.Status = types.ScalingOperationStatusFailed
	op.EndTime = time.Now()
	op.FailureReason = reason
	return c.store.Update(ctx, types.ResourceTypeScalingOperation, op.Namespace, op.ID, op)
}

// GetActiveOperation gets the active scaling operation for a service
func (c *scalingController) GetActiveOperation(ctx context.Context, namespace, serviceName string) (*types.ScalingOperation, error) {
	var operations []types.ScalingOperation
	err := c.store.List(ctx, types.ResourceTypeScalingOperation, namespace, &operations)
	if err != nil {
		return nil, err
	}

	var activeOp *types.ScalingOperation
	for _, op := range operations {
		if op.ServiceName == serviceName && op.Status == types.ScalingOperationStatusInProgress {
			if activeOp == nil || op.StartTime.After(activeOp.StartTime) {
				op := op // Create new variable to avoid loop variable capture
				activeOp = &op
			}
		}
	}

	return activeOp, nil
}

// recoverInProgressOperations finds and resumes any scaling operations that were in progress
// when the server was shut down or crashed
func (c *scalingController) recoverInProgressOperations(ctx context.Context) error {
	c.logger.Info("Checking for in-progress scaling operations to recover")

	// Get all operations across all namespaces
	var operations []types.ScalingOperation
	err := c.store.List(ctx, types.ResourceTypeScalingOperation, "", &operations)
	if err != nil {
		return fmt.Errorf("failed to list scaling operations: %w", err)
	}

	var recoveredCount int
	for _, op := range operations {
		// Skip operations that aren't in progress
		if op.Status != types.ScalingOperationStatusInProgress {
			continue
		}

		// First update the service's state to match current status
		var service types.Service
		if err := c.store.Get(ctx, types.ResourceTypeService, op.Namespace, op.ServiceName, &service); err != nil {
			// If the service no longer exists, mark the scaling op as failed so we stop recovering it
			c.logger.Error("Failed to get service for recovery",
				log.Str("service", op.ServiceName),
				log.Str("namespace", op.Namespace),
				log.Err(err))

			opCopy := op // avoid loop variable capture
			failReason := fmt.Sprintf("service %s/%s not found during recovery: %v", op.Namespace, op.ServiceName, err)
			if err := c.failOperation(ctx, &opCopy, failReason); err != nil {
				c.logger.Error("Failed to mark scaling operation as failed after missing service",
					log.Str("operation_id", op.ID),
					log.Err(err))
			}
			continue
		}

		c.logger.Info("Found in-progress scaling operation to recover",
			log.Str("service", op.ServiceName),
			log.Str("namespace", op.Namespace),
			log.Str("operation_id", op.ID),
			log.Int("current_service_scale", service.Scale),
			log.Int("target_scale", op.TargetScale),
			log.Str("start_time", op.StartTime.Format(time.RFC3339)))

		// Check if the operation is too old (avoid processing very old operations)
		// If operation is older than 1 hour, mark it as failed due to timeout
		if time.Since(op.StartTime) > time.Hour {
			c.logger.Warn("Found stale in-progress scaling operation, marking as failed",
				log.Str("service", op.ServiceName),
				log.Str("namespace", op.Namespace),
				log.Str("operation_id", op.ID),
				log.Int("current", op.CurrentScale),
				log.Int("target", op.TargetScale),
				log.Str("start_time", op.StartTime.Format(time.RFC3339)))

			opCopy := op // Create a copy to avoid issues with loop variable capture
			if err := c.failOperation(ctx, &opCopy, "Operation timed out during server recovery"); err != nil {
				c.logger.Error("Failed to mark stale operation as failed",
					log.Str("operation_id", op.ID),
					log.Err(err))
			}
			continue
		}

		// If we've already reached the target scale, just complete the operation
		if service.Scale == op.TargetScale {
			c.logger.Info("Service already at target scale, completing operation",
				log.Str("service", op.ServiceName),
				log.Str("operation_id", op.ID),
				log.Int("scale", service.Scale))

			opCopy := op // Create a copy to avoid issues with loop variable capture
			if err := c.completeOperation(ctx, &opCopy); err != nil {
				c.logger.Error("Failed to complete operation that reached target scale",
					log.Str("operation_id", op.ID),
					log.Err(err))
			}
			recoveredCount++
			continue
		}

		c.logger.Info("Recovering in-progress scaling operation",
			log.Str("service", op.ServiceName),
			log.Str("namespace", op.Namespace),
			log.Str("operation_id", op.ID),
			log.Int("current", op.CurrentScale),
			log.Int("target", op.TargetScale),
			log.Str("start_time", op.StartTime.Format(time.RFC3339)))

		// Process the scaling operation depending on mode
		if op.Mode == types.ScalingModeGradual {
			opCopy := op // Create a copy to avoid issues with loop variable capture
			if err := c.startGradualScaling(ctx, &opCopy); err != nil {
				c.logger.Error("Failed to recover gradual scaling operation",
					log.Str("service", op.ServiceName),
					log.Str("operation_id", op.ID),
					log.Err(err))

				// Mark as failed if we couldn't recover it
				if err := c.failOperation(ctx, &opCopy, fmt.Sprintf("Failed to recover: %v", err)); err != nil {
					c.logger.Error("Failed to mark operation as failed",
						log.Str("operation_id", op.ID),
						log.Err(err))
				}
			}
		} else {
			// For immediate scaling, just process it now
			opCopy := op // Create a copy to avoid issues with loop variable capture
			if err := c.executeImmediateScaling(ctx, &opCopy); err != nil {
				c.logger.Error("Failed to recover immediate scaling operation",
					log.Str("service", op.ServiceName),
					log.Str("operation_id", op.ID),
					log.Err(err))
				// Mark as failed so we don't repeatedly try to recover a broken op
				if err := c.failOperation(ctx, &opCopy, fmt.Sprintf("Failed to recover: %v", err)); err != nil {
					c.logger.Error("Failed to mark operation as failed",
						log.Str("operation_id", op.ID),
						log.Err(err))
				}
			}
		}

		recoveredCount++
	}

	c.logger.Info("Scaling operation recovery complete", log.Int("recovered", recoveredCount))
	return nil
}

// checkOperationsAtTargetScale looks for any in-progress operations where the target scale has already been reached
func (c *scalingController) checkOperationsAtTargetScale(ctx context.Context) {
	c.logger.Info("Checking for operations where target scale is already reached")

	// Get list of namespaces
	var namespaces []types.Namespace
	err := c.store.List(ctx, types.ResourceTypeNamespace, "", &namespaces)
	if err != nil {
		c.logger.Warn("Failed to list namespaces, will try default namespace", log.Err(err))
		namespaces = []types.Namespace{{Name: "default"}}
	}

	var completedCount int

	// Process each namespace
	for _, ns := range namespaces {
		// Get all in-progress operations for this namespace
		var operations []types.ScalingOperation
		err := c.store.List(ctx, types.ResourceTypeScalingOperation, ns.Name, &operations)
		if err != nil {
			c.logger.Error("Failed to list scaling operations",
				log.Str("namespace", ns.Name),
				log.Err(err))
			continue
		}

		c.logger.Debug("Checking operations in namespace",
			log.Str("namespace", ns.Name),
			log.Int("count", len(operations)))

		// Check each operation
		for _, op := range operations {
			// Skip operations that aren't in progress
			if op.Status != types.ScalingOperationStatusInProgress {
				continue
			}

			// Get the service
			var service types.Service
			if err := c.store.Get(ctx, types.ResourceTypeService, op.Namespace, op.ServiceName, &service); err != nil {
				c.logger.Error("Failed to get service for operation check",
					log.Str("service", op.ServiceName),
					log.Str("namespace", op.Namespace),
					log.Err(err))
				continue
			}

			// If service is already at target scale, complete the operation
			if service.Scale == op.TargetScale {
				c.logger.Info("Service already at target scale, completing operation",
					log.Str("operation_id", op.ID),
					log.Str("service", op.ServiceName),
					log.Int("scale", service.Scale))

				opCopy := op // Create a copy to avoid issues with variable capture
				if err := c.completeOperation(ctx, &opCopy); err != nil {
					c.logger.Error("Failed to complete operation at target scale",
						log.Str("operation_id", op.ID),
						log.Err(err))
				} else {
					completedCount++
				}
			}
		}
	}

	if completedCount > 0 {
		c.logger.Info("Completed operations that were already at target scale", log.Int("count", completedCount))
	}
}
