package tasks

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/finalizers"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/worker"
)

// DeletionTask implements the worker.Task interface for service deletion operations
type DeletionTask struct {
	*worker.BaseTask
	service           *types.Service
	request           *types.DeletionRequest
	finalizerTypes    []types.FinalizerType
	finalizerExecutor *finalizers.FinalizerExecutor
	store             store.Store
	logger            log.Logger
}

// NewDeletionTask creates a new deletion task
func NewDeletionTask(
	service *types.Service,
	request *types.DeletionRequest,
	finalizerTypes []types.FinalizerType,
	finalizerExecutor *finalizers.FinalizerExecutor,
	store store.Store,
	logger log.Logger,
) *DeletionTask {
	baseTask := &worker.BaseTask{
		ID:       fmt.Sprintf("delete-%s-%s-%d", service.Namespace, service.Name, time.Now().Unix()),
		Type:     "service-deletion",
		Priority: 0, // Normal priority
		Timeout:  10 * time.Minute,
		RetryPolicy: worker.RetryPolicy{
			MaxAttempts:       3,
			InitialDelay:      time.Second,
			MaxDelay:          time.Minute,
			BackoffMultiplier: 2.0,
		},
		Status:   worker.TaskStatusPending,
		Metadata: make(map[string]interface{}),
	}

	return &DeletionTask{
		BaseTask:          baseTask,
		service:           service,
		request:           request,
		finalizerTypes:    finalizerTypes,
		finalizerExecutor: finalizerExecutor,
		store:             store,
		logger:            logger.WithComponent("deletion-task"),
	}
}

// Execute implements the worker.Task interface
func (dt *DeletionTask) Execute(ctx context.Context) error {
	dt.logger.Info("Executing deletion task",
		log.Str("service", dt.service.Name),
		log.Str("namespace", dt.service.Namespace),
		log.Str("task_id", dt.GetID()))

	// Set task metadata
	dt.SetMetadata(map[string]interface{}{
		"service_name":      dt.service.Name,
		"service_namespace": dt.service.Namespace,
		"finalizer_count":   len(dt.finalizerTypes),
		"force_delete":      dt.request.Force,
		"dry_run":           dt.request.DryRun,
	})

	// Update deletion operation status to deleting instances
	if err := dt.updateDeletionOperation(ctx, func(op *types.DeletionOperation) {
		op.Status = types.DeletionOperationStatusDeletingInstances
	}); err != nil {
		dt.logger.Error("Failed to update deletion operation status", log.Err(err))
	}

	// Handle dry run
	if dt.request.DryRun {
		dt.logger.Info("Dry run deletion task completed",
			log.Str("service", dt.service.Name),
			log.Str("namespace", dt.service.Namespace))

		// Mark as completed for dry run
		if err := dt.updateDeletionOperation(ctx, func(op *types.DeletionOperation) {
			op.Status = types.DeletionOperationStatusCompleted
			now := time.Now()
			op.EndTime = &now
		}); err != nil {
			dt.logger.Error("Failed to mark deletion operation as completed", log.Err(err))
		}
		return nil
	}

	// Count instances before deletion for progress tracking
	initialInstanceCount, err := dt.countServiceInstances(ctx)
	if err != nil {
		dt.logger.Error("Failed to count initial instances", log.Err(err))
		// Continue anyway, we'll track what we can
	}

	// Update deletion operation with initial instance count
	if err := dt.updateDeletionOperation(ctx, func(op *types.DeletionOperation) {
		op.TotalInstances = initialInstanceCount
		op.Status = types.DeletionOperationStatusDeletingInstances
	}); err != nil {
		dt.logger.Error("Failed to update deletion operation with instance count", log.Err(err))
	}

	// Execute finalizers using the finalizer executor and get event channel
	eventChan, err := dt.finalizerExecutor.ExecuteFinalizers(ctx, dt.service, dt.finalizerTypes)
	if err != nil {
		dt.logger.Error("Deletion task failed",
			log.Str("service", dt.service.Name),
			log.Str("namespace", dt.service.Namespace),
			log.Str("task_id", dt.GetID()),
			log.Err(err))

		// Mark deletion operation as failed
		if updateErr := dt.updateDeletionOperation(ctx, func(op *types.DeletionOperation) {
			op.Status = types.DeletionOperationStatusFailed
			op.FailureReason = err.Error()
			now := time.Now()
			op.EndTime = &now
		}); updateErr != nil {
			dt.logger.Error("Failed to update deletion operation status", log.Err(updateErr))
		}

		return fmt.Errorf("finalizer execution failed: %w", err)
	}

	// Wait for finalizers to complete by listening to events
	dt.logger.Info("Waiting for finalizers to complete",
		log.Str("service", dt.service.Name),
		log.Str("namespace", dt.service.Namespace))

	// Block until all finalizers are complete
	dt.listenToFinalizerEvents(ctx, eventChan)

	// Count remaining instances after finalizers have completed
	remainingInstanceCount, err := dt.countServiceInstances(ctx)
	if err != nil {
		dt.logger.Error("Failed to count remaining instances", log.Err(err))
	}

	// Update deletion operation to completed
	if err := dt.updateDeletionOperation(ctx, func(op *types.DeletionOperation) {
		op.Status = types.DeletionOperationStatusCompleted
		op.DeletedInstances = initialInstanceCount - remainingInstanceCount
		now := time.Now()
		op.EndTime = &now
	}); err != nil {
		dt.logger.Error("Failed to mark deletion operation as completed", log.Err(err))
	}

	dt.logger.Info("Deletion task completed successfully",
		log.Str("service", dt.service.Name),
		log.Str("namespace", dt.service.Namespace),
		log.Str("task_id", dt.GetID()),
		log.Int("instances_deleted", initialInstanceCount-remainingInstanceCount))

	return nil
}

// listenToFinalizerEvents listens for finalizer events and updates the stored deletion operation in real-time
// This method blocks until all finalizers are complete
func (dt *DeletionTask) listenToFinalizerEvents(ctx context.Context, eventChan <-chan finalizers.FinalizerEvent) {
	finalizerCount := len(dt.finalizerTypes)
	completedFinalizers := 0
	failedFinalizers := 0

	dt.logger.Info("Starting to listen for finalizer events",
		log.Str("service", dt.service.Name),
		log.Str("namespace", dt.service.Namespace),
		log.Int("finalizer_count", finalizerCount))

	// If no finalizers, return immediately
	if finalizerCount == 0 {
		dt.logger.Info("No finalizers to wait for",
			log.Str("service", dt.service.Name),
			log.Str("namespace", dt.service.Namespace))
		return
	}

	for {
		select {
		case event := <-eventChan:
			dt.logger.Info("Finalizer event received",
				log.Str("finalizer_id", event.FinalizerID),
				log.Str("finalizer_type", string(event.FinalizerType)),
				log.Str("status", string(event.Status)),
				log.Str("error", event.Error))

			// Update the stored deletion operation with the event
			if err := dt.updateDeletionOperation(ctx, func(op *types.DeletionOperation) {
				// Find and update the finalizer
				for i := range op.Finalizers {
					if op.Finalizers[i].ID == event.FinalizerID {
						op.Finalizers[i].Status = event.Status
						op.Finalizers[i].UpdatedAt = event.Timestamp
						if event.Error != "" {
							op.Finalizers[i].Error = event.Error
						}
						if event.Status == types.FinalizerStatusCompleted {
							op.Finalizers[i].CompletedAt = &event.Timestamp
						}
						break
					}
				}
			}); err != nil {
				dt.logger.Error("Failed to update deletion operation with finalizer event", log.Err(err))
			}

			// Track completion status
			switch event.Status {
			case types.FinalizerStatusCompleted:
				completedFinalizers++
				dt.logger.Info("Finalizer completed",
					log.Str("finalizer_id", event.FinalizerID),
					log.Int("completed", completedFinalizers),
					log.Int("total", finalizerCount))
			case types.FinalizerStatusFailed:
				failedFinalizers++
				dt.logger.Error("Finalizer failed",
					log.Str("finalizer_id", event.FinalizerID),
					log.Str("error", event.Error))
			}

			// Check if all finalizers are done (completed or failed)
			if completedFinalizers+failedFinalizers >= finalizerCount {
				dt.logger.Info("All finalizers completed, returning from listener",
					log.Int("completed", completedFinalizers),
					log.Int("failed", failedFinalizers),
					log.Int("total", finalizerCount))
				return
			}

		case <-ctx.Done():
			dt.logger.Warn("Context cancelled while waiting for finalizers",
				log.Str("service", dt.service.Name),
				log.Str("namespace", dt.service.Namespace))
			return
		}
	}
}

// updateDeletionOperation updates the deletion operation in the store
func (dt *DeletionTask) updateDeletionOperation(ctx context.Context, updateFn func(*types.DeletionOperation)) error {
	// Get current deletion operation
	var deletionOp types.DeletionOperation
	if err := dt.store.Get(ctx, types.ResourceTypeDeletionOperation, dt.service.Namespace, dt.GetID(), &deletionOp); err != nil {
		return fmt.Errorf("failed to get deletion operation: %w", err)
	}

	// Apply update
	updateFn(&deletionOp)

	// Save back to store
	if err := dt.store.Update(ctx, types.ResourceTypeDeletionOperation, dt.service.Namespace, dt.GetID(), &deletionOp); err != nil {
		return fmt.Errorf("failed to update deletion operation: %w", err)
	}

	return nil
}

// countServiceInstances counts instances for this service
func (dt *DeletionTask) countServiceInstances(ctx context.Context) (int, error) {
	var instances []types.Instance
	if err := dt.store.List(ctx, types.ResourceTypeInstance, dt.service.Namespace, &instances); err != nil {
		return 0, fmt.Errorf("failed to list instances: %w", err)
	}

	count := 0
	for _, instance := range instances {
		if instance.ServiceName == dt.service.Name {
			count++
		}
	}

	return count, nil
}

// Validate implements the worker.Task interface
func (dt *DeletionTask) Validate() error {
	if dt.service == nil {
		return fmt.Errorf("service is required")
	}
	if dt.request == nil {
		return fmt.Errorf("deletion request is required")
	}
	if dt.finalizerExecutor == nil {
		return fmt.Errorf("finalizer executor is required")
	}
	if dt.store == nil {
		return fmt.Errorf("store is required")
	}
	return nil
}
