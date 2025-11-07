package finalizers

import (
	"context"
	"fmt"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// InstanceCleanupFinalizer handles cleanup of service instances
type InstanceCleanupFinalizer struct {
	*BaseFinalizer
	store              store.Store
	instanceController types.InstanceFinalizerInterface
	healthController   types.HealthFinalizerInterface
	logger             log.Logger
}

// NewInstanceCleanupFinalizer creates a new instance cleanup finalizer
func NewInstanceCleanupFinalizer(store store.Store, instanceController types.InstanceFinalizerInterface, healthController types.HealthFinalizerInterface, logger log.Logger) *InstanceCleanupFinalizer {
	return &InstanceCleanupFinalizer{
		BaseFinalizer: NewBaseFinalizer(
			types.FinalizerTypeInstanceCleanup,
			nil, // No dependencies
		),
		store:              store,
		instanceController: instanceController,
		healthController:   healthController,
		logger:             logger,
	}
}

// Execute performs the instance cleanup
func (f *InstanceCleanupFinalizer) Execute(ctx context.Context, service *types.Service) error {
	f.logger.Info("Starting instance cleanup",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace))

	// List all instances for this service
	var instances []types.Instance
	if err := f.store.List(ctx, types.ResourceTypeInstance, service.Namespace, &instances); err != nil {
		return fmt.Errorf("failed to list instances: %w", err)
	}

	// Filter instances for this service
	serviceInstances := make([]*types.Instance, 0)
	for _, instance := range instances {
		if instance.ServiceName == service.Name {
			serviceInstances = append(serviceInstances, &instance)
		}
	}

	f.logger.Info("Found instances to cleanup",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace),
		log.Int("count", len(serviceInstances)))

	// Stop and delete each instance
	for _, instance := range serviceInstances {
		f.logger.Info("Stopping instance",
			log.Str("service", service.Name),
			log.Str("instance", instance.ID))

		// Stop the instance
		if err := f.instanceController.StopInstance(ctx, instance); err != nil {
			f.logger.Error("Failed to stop instance",
				log.Str("service", service.Name),
				log.Str("instance", instance.ID),
				log.Err(err))
			// Continue with other instances even if one fails
		}

		// Delete the instance
		if err := f.instanceController.DeleteInstance(ctx, instance); err != nil {
			f.logger.Error("Failed to delete instance",
				log.Str("service", service.Name),
				log.Str("instance", instance.ID),
				log.Err(err))
			// Continue with other instances even if one fails
		}

		// Remove from health monitoring
		f.healthController.RemoveInstance(instance.ID)

		// Remove from store
		if err := f.store.Delete(ctx, types.ResourceTypeInstance, service.Namespace, instance.ID); err != nil {
			f.logger.Error("Failed to remove instance from store",
				log.Str("service", service.Name),
				log.Str("instance", instance.ID),
				log.Err(err))
			// Continue with other instances even if one fails
		}
	}

	f.logger.Info("Instance cleanup completed",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace))

	return nil
}

// Validate checks if the finalizer can be executed
func (f *InstanceCleanupFinalizer) Validate(service *types.Service) error {
	if service == nil {
		return fmt.Errorf("service cannot be nil")
	}
	return nil
}
