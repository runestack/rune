// Volume repository — namespaced CRUD over types.Volume.
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

// VolumeRepo provides CRUD over Volume resources.
type VolumeRepo struct {
	base *BaseRepo[types.Volume]
}

// NewVolumeRepo constructs a VolumeRepo.
func NewVolumeRepo(core store.Store) *VolumeRepo {
	return &VolumeRepo{
		base: NewBaseRepo[types.Volume](core, types.ResourceTypeVolume),
	}
}

// Create writes a new Volume row, populating ID, timestamps, and a default
// Pending status if the caller did not supply one.
func (r *VolumeRepo) Create(ctx context.Context, v *types.Volume) error {
	if v == nil {
		return fmt.Errorf("volume: nil")
	}
	if v.Namespace == "" {
		return fmt.Errorf("volume: namespace is required")
	}
	if v.Name == "" {
		return fmt.Errorf("volume: name is required")
	}
	if err := utils.ValidateDNS1123Name(v.Name); err != nil {
		return fmt.Errorf("volume name validation failed: %w", err)
	}
	if v.StorageClassName == "" {
		return fmt.Errorf("volume %q: storageClassName is required", v.Name)
	}
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	now := time.Now()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	if v.Status == "" {
		v.Status = types.VolumeStatusPending
	}
	return r.base.Create(ctx, v.Namespace, v.Name, v)
}

// Get retrieves a Volume by namespace and name.
func (r *VolumeRepo) Get(ctx context.Context, namespace, name string) (*types.Volume, error) {
	if namespace == "" || name == "" {
		return nil, fmt.Errorf("volume: namespace and name are required")
	}
	return r.base.Get(ctx, namespace, name)
}

// List returns every Volume in the given namespace.
func (r *VolumeRepo) List(ctx context.Context, namespace string) ([]*types.Volume, error) {
	return r.base.List(ctx, namespace)
}

// Update writes an updated Volume, preserving CreatedAt/ID and refreshing
// UpdatedAt.
func (r *VolumeRepo) Update(ctx context.Context, v *types.Volume, opts ...store.UpdateOption) error {
	if v == nil || v.Namespace == "" || v.Name == "" {
		return fmt.Errorf("volume: namespace and name are required")
	}
	cur, err := r.base.Get(ctx, v.Namespace, v.Name)
	if err != nil {
		return err
	}
	if v.ID == "" {
		v.ID = cur.ID
	}
	v.CreatedAt = cur.CreatedAt
	v.UpdatedAt = time.Now()
	return r.base.Update(ctx, v.Namespace, v.Name, v, opts...)
}

// Delete removes a Volume row. The orchestrator's VolumeController watches
// for the resulting DELETED event and runs the per-driver reclaim path.
func (r *VolumeRepo) Delete(ctx context.Context, namespace, name string) error {
	if namespace == "" || name == "" {
		return fmt.Errorf("volume: namespace and name are required")
	}
	return r.base.Delete(ctx, namespace, name)
}
