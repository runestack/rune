package finalizers

import (
	"context"
	"fmt"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// ServiceDeregisterFinalizer handles deregistering the service from service discovery
type ServiceDeregisterFinalizer struct {
	*BaseFinalizer
	store  store.Store
	logger log.Logger
}

// NewServiceDeregisterFinalizer creates a new service deregister finalizer
func NewServiceDeregisterFinalizer(store store.Store, logger log.Logger) *ServiceDeregisterFinalizer {
	return &ServiceDeregisterFinalizer{
		BaseFinalizer: NewBaseFinalizer(
			types.FinalizerTypeServiceDeregister,
			[]types.FinalizerDependency{
				{
					DependsOn: types.FinalizerTypeInstanceCleanup,
					Required:  true,
				},
			},
		),
		store:  store,
		logger: logger,
	}
}

// Execute performs the service deregistration
func (f *ServiceDeregisterFinalizer) Execute(ctx context.Context, service *types.Service) error {
	f.logger.Info("Starting service deregistration",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace))

	// Remove the service from the store
	// This is the final step after all instances have been cleaned up by InstanceCleanupFinalizer
	if err := f.store.Delete(ctx, types.ResourceTypeService, service.Namespace, service.Name); err != nil {
		return fmt.Errorf("failed to delete service from store: %w", err)
	}

	f.logger.Info("Service deregistration completed",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace))

	return nil
}

// Validate checks if the finalizer can be executed
func (f *ServiceDeregisterFinalizer) Validate(service *types.Service) error {
	if service == nil {
		return fmt.Errorf("service cannot be nil")
	}
	return nil
}
