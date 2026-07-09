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

	// Try to fetch as an instance — first by ID (fast path for UUIDs the
	// CLI pastes), then by Name. In the new naming scheme multiple records
	// share Names within a namespace (one live + zero-or-more Failed
	// tombstones), so the Name lookup picks the live record first,
	// falling back to the most-recent Failed tombstone.
	if instance, err := _store.GetInstanceByID(ctx, namespace, arg); err == nil {
		return ResourceTarget{Type: types.ResourceTypeInstance, Resource: instance}, nil
	}

	var allInstances []types.Instance
	if listErr := _store.List(ctx, types.ResourceTypeInstance, namespace, &allInstances); listErr == nil {
		var live *types.Instance
		var newestFailed *types.Instance
		for i := range allInstances {
			inst := &allInstances[i]
			if inst.Name != arg {
				continue
			}
			switch {
			case inst.Status != types.InstanceStatusFailed && inst.Status != types.InstanceStatusDeleted:
				live = inst
			case inst.Status == types.InstanceStatusFailed && inst.FailedAt != nil:
				if newestFailed == nil || inst.FailedAt.After(*newestFailed.FailedAt) {
					newestFailed = inst
				}
			}
		}
		winner := live
		if winner == nil {
			winner = newestFailed
		}
		if winner != nil {
			return ResourceTarget{Type: types.ResourceTypeInstance, Resource: winner}, nil
		}

		// Unique UUID-prefix match: `rune get instances` prints a short 8-hex
		// id, so accept it like a git/docker short id. Gated on a hex-looking
		// arg so it can't hijack a partial service/instance name.
		if isHexIDPrefix(arg) {
			var matches []*types.Instance
			for i := range allInstances {
				if strings.HasPrefix(allInstances[i].ID, arg) {
					matches = append(matches, &allInstances[i])
				}
			}
			if len(matches) == 1 {
				return ResourceTarget{Type: types.ResourceTypeInstance, Resource: matches[0]}, nil
			}
			if len(matches) > 1 {
				return ResourceTarget{}, fmt.Errorf("instance id prefix %q is ambiguous: %d instances match in namespace %s", arg, len(matches), namespace)
			}
		}
	}

	return ResourceTarget{}, fmt.Errorf("no service or instance %q found in namespace %s", arg, namespace)
}

// isHexIDPrefix reports whether s is a plausible UUID prefix: at least 6
// lowercase-hex characters (the table prints 8). Short of 6 the collision risk
// and the chance of shadowing a real name are too high to auto-resolve.
func isHexIDPrefix(s string) bool {
	if len(s) < 6 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
