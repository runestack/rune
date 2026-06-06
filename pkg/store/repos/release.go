package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

// ReleaseRepo persists stateful runeset releases (RUNESET_STATEFUL_RELEASES.md).
// A release is the tracked, revisioned record of what a `rune cast` installed.
type ReleaseRepo struct {
	base *BaseRepo[types.Release]
}

func NewReleaseRepo(core store.Store) *ReleaseRepo {
	return &ReleaseRepo{
		base: NewBaseRepo[types.Release](core, types.ResourceTypeRelease),
	}
}

// Create persists a new release record. Callers (the cast reconcile loop)
// supply Name/Namespace and the planned Owns set; this sets timestamps and
// sensible defaults.
func (r *ReleaseRepo) Create(ctx context.Context, rel *types.Release) error {
	if rel == nil || rel.Name == "" || rel.Namespace == "" {
		return fmt.Errorf("invalid release: name and namespace are required")
	}
	if err := utils.ValidateDNS1123Name(rel.Name); err != nil {
		return fmt.Errorf("release name validation failed: %w", err)
	}
	now := time.Now()
	if rel.CreatedAt.IsZero() {
		rel.CreatedAt = now
	}
	rel.UpdatedAt = now
	if rel.Revision == 0 {
		rel.Revision = 1
	}
	if rel.Status == "" {
		rel.Status = types.ReleaseStatusPending
	}
	return r.base.Create(ctx, rel.Namespace, rel.Name, rel)
}

// GetByName retrieves a release by name within a namespace.
func (r *ReleaseRepo) GetByName(ctx context.Context, namespace, name string) (*types.Release, error) {
	return r.base.Get(ctx, namespace, name)
}

// Get retrieves a release by resource ref (e.g. "release:name.namespace.rune").
func (r *ReleaseRepo) Get(ctx context.Context, ref string) (*types.Release, error) {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return nil, err
	}
	return r.base.Get(ctx, pr.Namespace, pr.Name)
}

// Update persists changes to an existing release, refreshing UpdatedAt.
// Revision is owned by the reconcile loop, not bumped here.
func (r *ReleaseRepo) Update(ctx context.Context, rel *types.Release, opts ...store.UpdateOption) error {
	if rel == nil || rel.Name == "" || rel.Namespace == "" {
		return fmt.Errorf("invalid release: name and namespace are required")
	}
	rel.UpdatedAt = time.Now()
	return r.base.Update(ctx, rel.Namespace, rel.Name, rel, opts...)
}

// SetStatus updates only the status of a release.
func (r *ReleaseRepo) SetStatus(ctx context.Context, namespace, name string, status types.ReleaseStatus) error {
	rel, err := r.GetByName(ctx, namespace, name)
	if err != nil {
		return err
	}
	rel.Status = status
	return r.Update(ctx, rel)
}

// Delete hard-removes the release record (used by `--purge`; soft uninstall
// instead sets Status=uninstalled via SetStatus — Decision D4).
func (r *ReleaseRepo) Delete(ctx context.Context, namespace, name string) error {
	return r.base.Delete(ctx, namespace, name)
}

// List returns releases within a namespace.
func (r *ReleaseRepo) List(ctx context.Context, namespace string) ([]*types.Release, error) {
	return r.base.List(ctx, namespace)
}

// ListAll returns releases across all namespaces (for `rune release list`).
func (r *ReleaseRepo) ListAll(ctx context.Context) ([]*types.Release, error) {
	var items []types.Release
	if err := r.base.Core().ListAll(ctx, types.ResourceTypeRelease, &items); err != nil {
		return nil, err
	}
	out := make([]*types.Release, 0, len(items))
	for i := range items {
		item := items[i]
		out = append(out, &item)
	}
	return out, nil
}

// History returns the revision history of a release for rollback/diff.
func (r *ReleaseRepo) History(ctx context.Context, ref string) ([]store.HistoricalVersion, error) {
	return r.base.GetHistory(ctx, ref)
}

// GetVersion returns a specific historical revision of a release.
func (r *ReleaseRepo) GetVersion(ctx context.Context, ref, version string) (*types.Release, error) {
	return r.base.GetVersion(ctx, ref, version)
}

// Watch proxies to the base store.
func (r *ReleaseRepo) Watch(ctx context.Context, namespace string) (<-chan store.WatchEvent, error) {
	return r.base.Watch(ctx, namespace)
}

// Core returns the underlying store.
func (r *ReleaseRepo) Core() store.Store { return r.base.Core() }
