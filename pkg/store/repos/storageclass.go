// Package repos — StorageClass and Volume repositories. RUNE-072.
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

// StorageClass is cluster-scoped: rows live under an empty namespace key,
// matching how the volume controller writes them.
const storageClassNamespaceKey = ""

// StorageClassRepo provides CRUD over StorageClass resources.
type StorageClassRepo struct {
	base *BaseRepo[types.StorageClass]
}

// NewStorageClassRepo constructs a StorageClassRepo.
func NewStorageClassRepo(core store.Store) *StorageClassRepo {
	return &StorageClassRepo{
		base: NewBaseRepo[types.StorageClass](core, types.ResourceTypeStorageClass),
	}
}

// Create writes a new StorageClass row, populating ID and timestamps.
func (r *StorageClassRepo) Create(ctx context.Context, sc *types.StorageClass) error {
	if sc == nil || sc.Name == "" {
		return fmt.Errorf("storage class: name is required")
	}
	if err := utils.ValidateDNS1123Name(sc.Name); err != nil {
		return fmt.Errorf("storage class name validation failed: %w", err)
	}
	if sc.Driver == "" {
		return fmt.Errorf("storage class %q: driver is required", sc.Name)
	}
	if sc.ID == "" {
		sc.ID = uuid.NewString()
	}
	now := time.Now()
	if sc.CreatedAt.IsZero() {
		sc.CreatedAt = now
	}
	sc.UpdatedAt = now
	return r.base.Create(ctx, storageClassNamespaceKey, sc.Name, sc)
}

// Get retrieves a StorageClass by name.
func (r *StorageClassRepo) Get(ctx context.Context, name string) (*types.StorageClass, error) {
	if name == "" {
		return nil, fmt.Errorf("storage class: name is required")
	}
	return r.base.Get(ctx, storageClassNamespaceKey, name)
}

// List returns every StorageClass.
func (r *StorageClassRepo) List(ctx context.Context) ([]*types.StorageClass, error) {
	return r.base.List(ctx, storageClassNamespaceKey)
}

// Update writes an updated StorageClass, refreshing UpdatedAt and preserving
// CreatedAt from the existing row.
func (r *StorageClassRepo) Update(ctx context.Context, sc *types.StorageClass, opts ...store.UpdateOption) error {
	if sc == nil || sc.Name == "" {
		return fmt.Errorf("storage class: name is required")
	}
	cur, err := r.base.Get(ctx, storageClassNamespaceKey, sc.Name)
	if err != nil {
		return err
	}
	sc.CreatedAt = cur.CreatedAt
	if sc.ID == "" {
		sc.ID = cur.ID
	}
	sc.UpdatedAt = time.Now()
	return r.base.Update(ctx, storageClassNamespaceKey, sc.Name, sc, opts...)
}

// Delete removes a StorageClass.
func (r *StorageClassRepo) Delete(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("storage class: name is required")
	}
	return r.base.Delete(ctx, storageClassNamespaceKey, name)
}
