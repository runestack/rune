package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

type InstanceRepo struct {
	base *BaseRepo[types.Instance]
}

type InstanceOption func(*InstanceRepo)

func NewInstanceRepo(core store.Store, opts ...InstanceOption) *InstanceRepo {
	repo := &InstanceRepo{
		base: NewBaseRepo[types.Instance](core, types.ResourceTypeInstance),
	}

	for _, opt := range opts {
		opt(repo)
	}
	return repo
}

// Create using fields on instance (no ref required)
func (r *InstanceRepo) Create(ctx context.Context, i *types.Instance) error {
	if i == nil || i.Name == "" || i.Namespace == "" {
		return fmt.Errorf("invalid instance")
	}
	if err := i.Validate(); err != nil {
		return fmt.Errorf("instance validation failed: %w", err)
	}
	now := time.Now()
	if i.CreatedAt.IsZero() {
		i.CreatedAt = now
	}
	i.UpdatedAt = now
	return r.base.Create(ctx, i.Namespace, i.ID, i)
}

func (r *InstanceRepo) CreateRef(ctx context.Context, ref string, i *types.Instance) error {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return err
	}
	if i != nil {
		if i.Namespace == "" {
			i.Namespace = pr.Namespace
		}
		if i.Name == "" {
			i.Name = pr.Name
		}
	}
	return r.Create(ctx, i)
}

func (r *InstanceRepo) Get(ctx context.Context, ref string) (*types.Instance, error) {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return nil, err
	}
	return r.base.Get(ctx, pr.Namespace, pr.Name)
}

// GetByName retrieves an instance by name and namespace
func (r *InstanceRepo) GetByName(ctx context.Context, namespace, name string) (*types.Instance, error) {
	return r.base.Get(ctx, namespace, name)
}

// GetByInstanceID retrieves an instance by its ID (not name)
func (r *InstanceRepo) GetByInstanceID(ctx context.Context, namespace, instanceID string) (*types.Instance, error) {
	return r.base.Get(ctx, namespace, instanceID)
}

func (r *InstanceRepo) Update(ctx context.Context, ref string, i *types.Instance, opts ...store.UpdateOption) error {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return err
	}
	cur, err := r.Get(ctx, ref)
	if err != nil {
		return err
	}
	if i.Namespace == "" {
		i.Namespace = pr.Namespace
	}
	if i.Name == "" {
		i.Name = pr.Name
	}
	if err := i.Validate(); err != nil {
		return fmt.Errorf("instance validation failed: %w", err)
	}
	i.CreatedAt = cur.CreatedAt
	i.UpdatedAt = time.Now()
	return r.base.Update(ctx, pr.Namespace, pr.Name, i, opts...)
}

func (r *InstanceRepo) Delete(ctx context.Context, ref string) error {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return err
	}
	return r.base.Delete(ctx, pr.Namespace, pr.Name)
}

func (r *InstanceRepo) List(ctx context.Context, namespace string) ([]*types.Instance, error) {
	return r.base.List(ctx, namespace)
}

// ListByService returns all instances belonging to a specific service
func (r *InstanceRepo) ListByService(ctx context.Context, namespace, serviceID string) ([]*types.Instance, error) {
	instances, err := r.List(ctx, namespace)
	if err != nil {
		return nil, err
	}

	var filtered []*types.Instance
	for _, instance := range instances {
		if instance.ServiceID == serviceID {
			filtered = append(filtered, instance)
		}
	}
	return filtered, nil
}

// ListByServiceName returns all instances belonging to a service by name
func (r *InstanceRepo) ListByServiceName(ctx context.Context, namespace, serviceName string) ([]*types.Instance, error) {
	instances, err := r.List(ctx, namespace)
	if err != nil {
		return nil, err
	}

	var filtered []*types.Instance
	for _, instance := range instances {
		if instance.ServiceName == serviceName {
			filtered = append(filtered, instance)
		}
	}
	return filtered, nil
}

// ListByStatus returns instances with the given status
func (r *InstanceRepo) ListByStatus(ctx context.Context, namespace string, status types.InstanceStatus) ([]*types.Instance, error) {
	instances, err := r.List(ctx, namespace)
	if err != nil {
		return nil, err
	}

	var filtered []*types.Instance
	for _, instance := range instances {
		if instance.Status == status {
			filtered = append(filtered, instance)
		}
	}

	return filtered, nil
}

// ListByNode returns all instances running on a specific node
func (r *InstanceRepo) ListByNode(ctx context.Context, namespace, nodeID string) ([]*types.Instance, error) {
	instances, err := r.List(ctx, namespace)
	if err != nil {
		return nil, err
	}

	var filtered []*types.Instance
	for _, instance := range instances {
		if instance.NodeID == nodeID {
			filtered = append(filtered, instance)
		}
	}
	return filtered, nil
}

// ListRunning returns all running instances in a namespace
func (r *InstanceRepo) ListRunning(ctx context.Context, namespace string) ([]*types.Instance, error) {
	return r.ListByStatus(ctx, namespace, types.InstanceStatusRunning)
}

// ListPending returns all pending instances in a namespace
func (r *InstanceRepo) ListPending(ctx context.Context, namespace string) ([]*types.Instance, error) {
	return r.ListByStatus(ctx, namespace, types.InstanceStatusPending)
}

// ListFailed returns all failed instances in a namespace
func (r *InstanceRepo) ListFailed(ctx context.Context, namespace string) ([]*types.Instance, error) {
	return r.ListByStatus(ctx, namespace, types.InstanceStatusFailed)
}

// UpdateStatus updates only the status of an instance
func (r *InstanceRepo) UpdateStatus(ctx context.Context, ref string, status types.InstanceStatus, statusMessage string) error {
	instance, err := r.Get(ctx, ref)
	if err != nil {
		return err
	}
	instance.Status = status
	instance.StatusMessage = statusMessage
	instance.UpdatedAt = time.Now()
	return r.Update(ctx, ref, instance)
}

// UpdateIP updates only the IP address of an instance
func (r *InstanceRepo) UpdateIP(ctx context.Context, ref string, ip string) error {
	instance, err := r.Get(ctx, ref)
	if err != nil {
		return err
	}
	instance.IP = ip
	instance.UpdatedAt = time.Now()
	return r.Update(ctx, ref, instance)
}

// UpdateContainerID updates only the container ID of an instance
func (r *InstanceRepo) UpdateContainerID(ctx context.Context, ref string, containerID string) error {
	instance, err := r.Get(ctx, ref)
	if err != nil {
		return err
	}
	instance.ContainerID = containerID
	instance.UpdatedAt = time.Now()
	return r.Update(ctx, ref, instance)
}

// UpdatePID updates only the process ID of an instance
func (r *InstanceRepo) UpdatePID(ctx context.Context, ref string, pid int) error {
	instance, err := r.Get(ctx, ref)
	if err != nil {
		return err
	}
	instance.PID = pid
	instance.UpdatedAt = time.Now()
	return r.Update(ctx, ref, instance)
}

// IncrementRestartCount increments the restart count for an instance
func (r *InstanceRepo) IncrementRestartCount(ctx context.Context, ref string) error {
	instance, err := r.Get(ctx, ref)
	if err != nil {
		return err
	}
	if instance.Metadata == nil {
		instance.Metadata = &types.InstanceMetadata{}
	}
	instance.Metadata.RestartCount++
	instance.UpdatedAt = time.Now()
	return r.Update(ctx, ref, instance)
}

// MarkForDeletion marks an instance for deletion
func (r *InstanceRepo) MarkForDeletion(ctx context.Context, ref string) error {
	instance, err := r.Get(ctx, ref)
	if err != nil {
		return err
	}
	instance.Status = types.InstanceStatusDeleted
	now := time.Now()
	instance.UpdatedAt = now
	if instance.Metadata == nil {
		instance.Metadata = &types.InstanceMetadata{}
	}
	instance.Metadata.DeletionTimestamp = &now
	return r.Update(ctx, ref, instance)
}

// GetServiceInstances returns all instances for a service across all namespaces
func (r *InstanceRepo) GetServiceInstances(ctx context.Context, serviceID string) ([]*types.Instance, error) {
	// This would need to be implemented differently if we want to search across namespaces
	// For now, we'll search in the default namespace
	instances, err := r.List(ctx, "default")
	if err != nil {
		return nil, err
	}

	var filtered []*types.Instance
	for _, instance := range instances {
		if instance.ServiceID == serviceID {
			filtered = append(filtered, instance)
		}
	}
	return filtered, nil
}

// CountByService returns the count of instances for a specific service
func (r *InstanceRepo) CountByService(ctx context.Context, namespace, serviceID string) (int, error) {
	instances, err := r.ListByService(ctx, namespace, serviceID)
	if err != nil {
		return 0, err
	}
	return len(instances), nil
}

// CountByStatus returns the count of instances with a specific status
func (r *InstanceRepo) CountByStatus(ctx context.Context, namespace string, status types.InstanceStatus) (int, error) {
	instances, err := r.ListByStatus(ctx, namespace, status)
	if err != nil {
		return 0, err
	}
	return len(instances), nil
}

// Watch proxies to the base store
func (r *InstanceRepo) Watch(ctx context.Context, namespace string) (<-chan store.WatchEvent, error) {
	return r.base.Watch(ctx, namespace)
}

// GetHistory returns version history for an instance
func (r *InstanceRepo) GetHistory(ctx context.Context, ref string) ([]store.HistoricalVersion, error) {
	return r.base.GetHistory(ctx, ref)
}

// GetVersion returns a specific version of an instance
func (r *InstanceRepo) GetVersion(ctx context.Context, ref string, version string) (*types.Instance, error) {
	return r.base.GetVersion(ctx, ref, version)
}

// Core returns the underlying store
func (r *InstanceRepo) Core() store.Store { return r.base.Core() }
