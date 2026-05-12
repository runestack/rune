// Snapshot repository — namespaced CRUD over types.Snapshot.
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

// SnapshotRepo provides CRUD over Snapshot resources.
type SnapshotRepo struct {
	base *BaseRepo[types.Snapshot]
}

// NewSnapshotRepo constructs a SnapshotRepo.
func NewSnapshotRepo(core store.Store) *SnapshotRepo {
	return &SnapshotRepo{
		base: NewBaseRepo[types.Snapshot](core, types.ResourceTypeSnapshot),
	}
}

// Create writes a new Snapshot row, populating ID, timestamps, and a
// default Pending phase if the caller did not supply one.
func (r *SnapshotRepo) Create(ctx context.Context, s *types.Snapshot) error {
	if s == nil {
		return fmt.Errorf("snapshot: nil")
	}
	if s.Namespace == "" {
		return fmt.Errorf("snapshot: namespace is required")
	}
	if s.Name == "" {
		return fmt.Errorf("snapshot: name is required")
	}
	if err := utils.ValidateDNS1123Name(s.Name); err != nil {
		return fmt.Errorf("snapshot name validation failed: %w", err)
	}
	if s.SourceVolume == "" {
		return fmt.Errorf("snapshot %q: sourceVolume is required", s.Name)
	}
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	now := time.Now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	if s.Phase == "" {
		s.Phase = types.SnapshotPhasePending
	}
	return r.base.Create(ctx, s.Namespace, s.Name, s)
}

// Get retrieves a Snapshot by namespace and name.
func (r *SnapshotRepo) Get(ctx context.Context, namespace, name string) (*types.Snapshot, error) {
	if namespace == "" || name == "" {
		return nil, fmt.Errorf("snapshot: namespace and name are required")
	}
	return r.base.Get(ctx, namespace, name)
}

// List returns every Snapshot in the given namespace.
func (r *SnapshotRepo) List(ctx context.Context, namespace string) ([]*types.Snapshot, error) {
	return r.base.List(ctx, namespace)
}

// ListAll returns every Snapshot across every namespace. Mirrors
// VolumeRepo.ListAll: enumerates via NamespaceRepo + per-namespace
// List so it works on backends that don't support cross-namespace
// listing.
func (r *SnapshotRepo) ListAll(ctx context.Context) ([]*types.Snapshot, error) {
	nsRepo := NewNamespaceRepo(r.base.core)
	namespaces, err := nsRepo.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []*types.Snapshot
	for _, ns := range namespaces {
		if ns == nil || ns.Name == "" {
			continue
		}
		snaps, err := r.base.List(ctx, ns.Name)
		if err != nil {
			continue
		}
		out = append(out, snaps...)
	}
	return out, nil
}

// Update writes an updated Snapshot, preserving CreatedAt/ID and
// refreshing UpdatedAt.
func (r *SnapshotRepo) Update(ctx context.Context, s *types.Snapshot, opts ...store.UpdateOption) error {
	if s == nil || s.Namespace == "" || s.Name == "" {
		return fmt.Errorf("snapshot: namespace and name are required")
	}
	cur, err := r.base.Get(ctx, s.Namespace, s.Name)
	if err != nil {
		return err
	}
	if s.ID == "" {
		s.ID = cur.ID
	}
	s.CreatedAt = cur.CreatedAt
	s.UpdatedAt = time.Now()
	return r.base.Update(ctx, s.Namespace, s.Name, s, opts...)
}

// Delete removes a Snapshot row. The orchestrator's SnapshotController
// watches for the resulting DELETED event and runs the per-driver
// reclaim path.
func (r *SnapshotRepo) Delete(ctx context.Context, namespace, name string) error {
	if namespace == "" || name == "" {
		return fmt.Errorf("snapshot: namespace and name are required")
	}
	return r.base.Delete(ctx, namespace, name)
}
