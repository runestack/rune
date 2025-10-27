package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

type ServiceRepo struct {
	base *BaseRepo[types.Service]
}

type ServiceOption func(*ServiceRepo)

func NewServiceRepo(core store.Store, opts ...ServiceOption) *ServiceRepo {
	repo := &ServiceRepo{
		base: NewBaseRepo[types.Service](core, types.ResourceTypeService),
	}

	for _, opt := range opts {
		opt(repo)
	}
	return repo
}

// Create using fields on service (no ref required)
func (r *ServiceRepo) Create(ctx context.Context, s *types.Service) error {
	if s == nil || s.Name == "" || s.Namespace == "" {
		return fmt.Errorf("invalid service")
	}

	if err := utils.ValidateDNS1123Name(s.Name); err != nil {
		return fmt.Errorf("service name validation failed: %w", err)
	}

	if err := s.Validate(); err != nil {
		return fmt.Errorf("service validation failed: %w", err)
	}
	now := time.Now()
	if s.Metadata == nil {
		s.Metadata = &types.ServiceMetadata{}
	}
	if s.Metadata.CreatedAt.IsZero() {
		s.Metadata.CreatedAt = now
	}
	s.Metadata.UpdatedAt = now
	s.Metadata.Generation = 1
	return r.base.Create(ctx, s.Namespace, s.Name, s)
}

func (r *ServiceRepo) CreateRef(ctx context.Context, ref string, s *types.Service) error {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return err
	}
	if s != nil {
		if s.Namespace == "" {
			s.Namespace = pr.Namespace
		}
		if s.Name == "" {
			s.Name = pr.Name
		}
	}
	return r.Create(ctx, s)
}

func (r *ServiceRepo) Get(ctx context.Context, ref string) (*types.Service, error) {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return nil, err
	}
	return r.base.Get(ctx, pr.Namespace, pr.Name)
}

// GetByName retrieves a service by name and namespace
func (r *ServiceRepo) GetByName(ctx context.Context, namespace, name string) (*types.Service, error) {
	return r.base.Get(ctx, namespace, name)
}

func (r *ServiceRepo) Update(ctx context.Context, ref string, s *types.Service, opts ...store.UpdateOption) error {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return err
	}
	cur, err := r.Get(ctx, ref)
	if err != nil {
		return err
	}
	next := cur.Metadata.Generation + 1
	if s.Namespace == "" {
		s.Namespace = pr.Namespace
	}
	if s.Name == "" {
		s.Name = pr.Name
	}
	if err := s.Validate(); err != nil {
		return fmt.Errorf("service validation failed: %w", err)
	}
	s.Metadata.CreatedAt = cur.Metadata.CreatedAt
	s.Metadata.UpdatedAt = time.Now()
	s.Metadata.Generation = next
	return r.base.Update(ctx, pr.Namespace, pr.Name, s, opts...)
}

func (r *ServiceRepo) Delete(ctx context.Context, ref string) error {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return err
	}
	return r.base.Delete(ctx, pr.Namespace, pr.Name)
}

func (r *ServiceRepo) List(ctx context.Context, namespace string) ([]*types.Service, error) {
	return r.base.List(ctx, namespace)
}

// GetByServiceID retrieves a service by its ID (not name)
func (r *ServiceRepo) GetByServiceID(ctx context.Context, namespace, serviceID string) (*types.Service, error) {
	services, err := r.List(ctx, namespace)
	if err != nil {
		return nil, err
	}
	for _, service := range services {
		if service.ID == serviceID {
			return service, nil
		}
	}
	return nil, fmt.Errorf("service with ID %s not found in namespace %s", serviceID, namespace)
}

// ListByLabelSelector returns services matching the given label selector
func (r *ServiceRepo) ListByLabelSelector(ctx context.Context, namespace string, selector map[string]string) ([]*types.Service, error) {
	services, err := r.List(ctx, namespace)
	if err != nil {
		return nil, err
	}

	var filtered []*types.Service
	for _, service := range services {
		if matchesLabelSelector(service.Labels, selector) {
			filtered = append(filtered, service)
		}
	}
	return filtered, nil
}

// ListByStatus returns services with the given status
func (r *ServiceRepo) ListByStatus(ctx context.Context, namespace string, status types.ServiceStatus) ([]*types.Service, error) {
	services, err := r.List(ctx, namespace)
	if err != nil {
		return nil, err
	}

	var filtered []*types.Service
	for _, service := range services {
		if service.Status == status {
			filtered = append(filtered, service)
		}
	}
	return filtered, nil
}

// UpdateStatus updates only the status of a service
func (r *ServiceRepo) UpdateStatus(ctx context.Context, ref string, status types.ServiceStatus) error {
	service, err := r.Get(ctx, ref)
	if err != nil {
		return err
	}
	service.Status = status
	service.Metadata.UpdatedAt = time.Now()
	return r.Update(ctx, ref, service)
}

// UpdateStatusByName updates only the status of a service by name and namespace
func (r *ServiceRepo) UpdateStatusByName(ctx context.Context, namespace, name string, status types.ServiceStatus) error {
	service, err := r.GetByName(ctx, namespace, name)
	if err != nil {
		return err
	}
	service.Status = status
	service.Metadata.UpdatedAt = time.Now()
	return r.Update(ctx, fmt.Sprintf("service:%s.%s.rune", name, namespace), service)
}

// UpdateScale updates only the scale of a service
func (r *ServiceRepo) UpdateScale(ctx context.Context, ref string, scale int) error {
	service, err := r.Get(ctx, ref)
	if err != nil {
		return err
	}
	service.Scale = scale
	service.Metadata.UpdatedAt = time.Now()
	service.Metadata.LastNonZeroScale = scale
	return r.Update(ctx, ref, service)
}

// GetDependents returns all services that depend on the given service
func (r *ServiceRepo) GetDependents(ctx context.Context, ref string) ([]*types.Service, error) {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return nil, err
	}

	services, err := r.List(ctx, pr.Namespace)
	if err != nil {
		return nil, err
	}

	var dependents []*types.Service
	for _, service := range services {
		for _, dep := range service.Dependencies {
			if dep.Service == pr.Name && dep.Namespace == pr.Namespace {
				dependents = append(dependents, service)
				break
			}
		}
	}
	return dependents, nil
}

// Watch proxies to the base store
func (r *ServiceRepo) Watch(ctx context.Context, namespace string) (<-chan store.WatchEvent, error) {
	return r.base.Watch(ctx, namespace)
}

// GetHistory returns version history for a service
func (r *ServiceRepo) GetHistory(ctx context.Context, ref string) ([]store.HistoricalVersion, error) {
	return r.base.GetHistory(ctx, ref)
}

// GetVersion returns a specific version of a service
func (r *ServiceRepo) GetVersion(ctx context.Context, ref string, version string) (*types.Service, error) {
	return r.base.GetVersion(ctx, ref, version)
}

// Core returns the underlying store
func (r *ServiceRepo) Core() store.Store { return r.base.Core() }

// matchesLabelSelector checks if the given labels match the selector
func matchesLabelSelector(labels, selector map[string]string) bool {
	if len(selector) == 0 {
		return true
	}

	for key, value := range selector {
		if labelValue, exists := labels[key]; !exists || labelValue != value {
			return false
		}
	}
	return true
}
