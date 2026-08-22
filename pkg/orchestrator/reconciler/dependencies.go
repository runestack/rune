// service dependency readiness checks.

package reconciler

import (
	"context"
	"fmt"

	"github.com/runestack/rune/pkg/types"
)

// dependenciesReady evaluates whether all declared dependencies for a service are ready.
// Readiness definition (MVP):
// - If dependency is not a service, it's ready
// - If dependency is a service and it defines readiness probe: at least one instance is Running and readiness=true
// - Else: at least one instance is Running
func (r *Reconciler) dependenciesReady(ctx context.Context, service *types.Service) (bool, error) {
	for _, dep := range service.Dependencies {
		// Fetch dependency service
		dependencyResource, err := r.fetchDependencyResource(ctx, &dep, service)
		if err != nil {
			return false, err
		}

		// If dependency is not a service, it's ready
		depService, ok := dependencyResource.(types.Service)
		if !ok {
			continue
		}

		// List instances for dependency
		instances, err := r.listInstancesForService(ctx, depService.Namespace, dep.Service)
		if err != nil {
			return false, fmt.Errorf("failed to list instances for dependency %s/%s: %w", depService.Namespace, dep.Service, err)
		}

		// Evaluate readiness
		readyFound := false
		hasReadinessProbe := depService.Health != nil && depService.Health.Readiness != nil
		for _, inst := range instances {
			if inst.Status != types.InstanceStatusRunning {
				continue
			}
			if !hasReadinessProbe {
				// Running instance is sufficient
				readyFound = true
				break
			}
			// Check readiness via health controller
			status, err := r.healthController.GetHealthStatus(ctx, inst.ID)
			if err != nil {
				// On error, treat as not ready; continue
				continue
			}
			if status != nil && status.Readiness {
				readyFound = true
				break
			}
		}
		if !readyFound {
			return false, nil
		}
	}
	return true, nil
}

func (r *Reconciler) fetchDependencyResource(ctx context.Context, dep *types.DependencyRef, service *types.Service) (interface{}, error) {
	var dependencyResource interface{}
	depNS := dep.Namespace
	if depNS == "" {
		depNS = service.Namespace
	}

	depResourceType := dep.GetDependencyResourceType()
	depResourceName := dep.GetDependencyResourceName()

	// For dependencies, fetch into concrete types so the store can unmarshal correctly
	switch depResourceType {
	case types.ResourceTypeService:
		var svc types.Service
		// Use explicit service name if provided, else fall back to computed name
		name := dep.Service
		if name == "" {
			name = depResourceName
		}
		if err := r.store.Get(ctx, types.ResourceTypeService, depNS, name, &svc); err != nil {
			return nil, fmt.Errorf("failed to get dependency %s/%s: %w", depNS, name, err)
		}
		return svc, nil
	case types.ResourceTypeConfigmap:
		var cfg types.Configmap
		if err := r.store.Get(ctx, depResourceType, depNS, depResourceName, &cfg); err != nil {
			return nil, fmt.Errorf("failed to get dependency %s/%s: %w", depNS, depResourceName, err)
		}
		return cfg, nil
	case types.ResourceTypeSecret:
		var sec types.Secret
		if err := r.store.Get(ctx, depResourceType, depNS, depResourceName, &sec); err != nil {
			return nil, fmt.Errorf("failed to get dependency %s/%s: %w", depNS, depResourceName, err)
		}
		return sec, nil
	default:
		if err := r.store.Get(ctx, depResourceType, depNS, depResourceName, &dependencyResource); err != nil {
			return nil, fmt.Errorf("failed to get dependency %s/%s: %w", depNS, depResourceName, err)
		}
		return dependencyResource, nil
	}
}
