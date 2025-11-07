package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

type NamespaceRepo struct {
	base *BaseRepo[types.Namespace]
}

type NamespaceOption func(*NamespaceRepo)

func NewNamespaceRepo(core store.Store, opts ...NamespaceOption) *NamespaceRepo {
	repo := &NamespaceRepo{
		base: NewBaseRepo[types.Namespace](core, types.ResourceTypeNamespace),
	}

	for _, opt := range opts {
		opt(repo)
	}
	return repo
}

// CreateBuiltIns creates a new namespace
func (r *NamespaceRepo) CreateBuiltIns(ctx context.Context, ns *types.Namespace) error {
	ns.ID = uuid.NewString()
	now := time.Now()
	ns.CreatedAt = now
	ns.UpdatedAt = now

	return r.base.Create(ctx, "system", ns.Name, ns)
}

// Create creates a new namespace
func (r *NamespaceRepo) Create(ctx context.Context, ns *types.Namespace) error {
	if ns == nil || ns.Name == "" {
		return fmt.Errorf("invalid namespace: name is required")
	}

	// Validate namespace name
	if err := r.validateNamespaceName(ns.Name); err != nil {
		return err
	}

	// Set ID if not provided
	if ns.ID == "" {
		ns.ID = uuid.NewString()
	}

	// Set timestamps
	now := time.Now()
	if ns.CreatedAt.IsZero() {
		ns.CreatedAt = now
	}
	ns.UpdatedAt = now

	return r.base.Create(ctx, "system", ns.Name, ns)
}

// Get retrieves a namespace by name
func (r *NamespaceRepo) Get(ctx context.Context, name string) (*types.Namespace, error) {
	if name == "" {
		return nil, fmt.Errorf("namespace name is required")
	}
	return r.base.Get(ctx, "system", name)
}

// List returns all namespaces
func (r *NamespaceRepo) List(ctx context.Context) ([]*types.Namespace, error) {
	return r.base.List(ctx, "system")
}

// Update updates an existing namespace
func (r *NamespaceRepo) Update(ctx context.Context, ns *types.Namespace) error {
	if ns == nil || ns.Name == "" {
		return fmt.Errorf("invalid namespace: name is required")
	}

	// Validate namespace name
	if err := r.validateNamespaceName(ns.Name); err != nil {
		return err
	}

	// Update timestamp
	ns.UpdatedAt = time.Now()

	return r.base.Update(ctx, "system", ns.Name, ns)
}

// Delete removes a namespace
func (r *NamespaceRepo) Delete(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("namespace name is required")
	}

	// Check if namespace is reserved
	if r.isReservedNamespace(name) {
		return fmt.Errorf("cannot delete reserved namespace: %s", name)
	}

	return r.base.Delete(ctx, "system", name)
}

// Exists checks if a namespace exists
func (r *NamespaceRepo) Exists(ctx context.Context, name string) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("namespace name is required")
	}

	_, err := r.Get(ctx, name)
	if err != nil {
		if store.IsNotFoundError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Watch watches for namespace changes
func (r *NamespaceRepo) Watch(ctx context.Context) (<-chan store.WatchEvent, error) {
	return r.base.Watch(ctx, "system")
}

// validateNamespaceName validates the namespace name according to DNS-1123 rules
func (r *NamespaceRepo) validateNamespaceName(name string) error {
	// Check if it's a reserved namespace
	if r.isReservedNamespace(name) {
		return fmt.Errorf("namespace name '%s' is reserved and cannot be used", name)
	}

	return utils.ValidateDNS1123Name(name)
}

// isReservedNamespace checks if the namespace name is reserved
func (r *NamespaceRepo) isReservedNamespace(name string) bool {
	reserved := []string{"system", "default"}
	for _, reservedName := range reserved {
		if name == reservedName {
			return true
		}
	}
	return false
}

// BootstrapDefaultNamespaces creates the default and system namespaces if they don't exist
func (r *NamespaceRepo) BootstrapDefaultNamespaces(ctx context.Context) error {
	defaultNamespaces := []string{"default", "system"}

	for _, name := range defaultNamespaces {
		exists, err := r.Exists(ctx, name)
		if err != nil {
			return fmt.Errorf("failed to check if namespace %s exists: %w", name, err)
		}

		if !exists {
			ns := &types.Namespace{
				Name:      name,
				Labels:    map[string]string{"rune.io/reserved": "true"},
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}

			if err := r.Create(ctx, ns); err != nil {
				return fmt.Errorf("failed to create namespace %s: %w", name, err)
			}
		}
	}

	return nil
}
