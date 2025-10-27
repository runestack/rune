package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

type ResourceTarget struct {
	Type     types.ResourceType
	Resource interface{}
}

func (r ResourceTarget) GetService() (*types.Service, error) {
	if r.Type != types.ResourceTypeService {
		return nil, fmt.Errorf("resource is not a service")
	}

	service, ok := r.Resource.(*types.Service)
	if !ok {
		return nil, fmt.Errorf("resource is not a service")
	}

	return service, nil
}

func (r ResourceTarget) GetInstance() (*types.Instance, error) {
	if r.Type != types.ResourceTypeInstance {
		return nil, fmt.Errorf("resource is not an instance")
	}
	instance, ok := r.Resource.(*types.Instance)
	if !ok {
		return nil, fmt.Errorf("resource is not an instance")
	}
	return instance, nil
}

// resolveResourceTarget attempts to identify the type of resource being queried
// It checks if the argument is service, instance, or type/name format
// Returns resource type and resource, and any error that occurred
func resolveResourceTarget(ctx context.Context, _store store.Store, arg string, namespace string) (ResourceTarget, error) {
	// Check if the argument is in the format TYPE/NAME
	if strings.Contains(arg, "/") {
		parts := strings.SplitN(arg, "/", 2)
		resourceType := strings.ToLower(parts[0])
		resourceName := parts[1]

		resource, err := getResourceByType(ctx, _store, resourceType, resourceName, namespace)
		if err != nil {
			return ResourceTarget{}, err
		}

		return ResourceTarget{Type: types.ResourceType(resourceType), Resource: resource}, nil
	}

	// Try to fetch as a service first
	var service types.Service
	err := _store.Get(ctx, types.ResourceTypeService, namespace, arg, &service)
	if err == nil {
		// It's a service
		return ResourceTarget{Type: types.ResourceTypeService, Resource: &service}, nil
	}

	// Try to fetch as an instance
	instance, err := _store.GetInstanceByID(ctx, namespace, arg)
	if err != nil {
		return ResourceTarget{}, err
	}

	return ResourceTarget{Type: types.ResourceTypeInstance, Resource: instance}, nil
}

func getResourceByType(ctx context.Context, _store store.Store, resourceType string, resourceName string, namespace string) (interface{}, error) {

	serviceTargetExpressions := []string{"service", "svc"}
	instanceTargetExpressions := []string{"instance", "inst"}

	if utils.SliceContains(serviceTargetExpressions, resourceType) {
		var service types.Service
		err := _store.Get(ctx, types.ResourceTypeService, namespace, resourceName, &service)
		if err == nil {
			return &service, nil
		}
	}

	if utils.SliceContains(instanceTargetExpressions, resourceType) {
		instance, err := _store.GetInstanceByID(ctx, namespace, resourceName)
		if err == nil {
			return instance, nil
		}
	}

	return nil, fmt.Errorf("resource not found")
}

func ensureNamespaceExists(ctx context.Context, nsRepo *repos.NamespaceRepo, namespace string, ensureNamespace bool) error {
	// Check if it's a reserved namespace
	if namespace == "system" || namespace == "default" {
		return nil
	}

	// Check if the namespace exists in the store
	_, err := nsRepo.Get(ctx, namespace)
	if err != nil {
		if store.IsNotFoundError(err) {
			// Namespace doesn't exist, check if we should create it
			if ensureNamespace {
				// Create the namespace
				newNs := &types.Namespace{
					Name:      namespace,
					Labels:    map[string]string{"rune/auto-created": "true"},
					CreatedAt: time.Now(),
					UpdatedAt: time.Now(),
				}

				if err := nsRepo.Create(ctx, newNs); err != nil {
					return fmt.Errorf("failed to create namespace %s: %w", namespace, err)
				}
				return nil
			} else {
				return fmt.Errorf("namespace %s does not exist (use --create-namespace to create it)", namespace)
			}
		}
		// Some other error occurred
		return fmt.Errorf("failed to check namespace %s: %w", namespace, err)
	}

	// Namespace exists, nothing to do
	return nil
}
