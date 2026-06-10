package repos

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

// SavedViews are cluster-scoped (one shared list, attributed via CreatedBy):
// rows live under an empty namespace key, mirroring the StorageClass pattern.
const savedViewNamespaceKey = ""

// maxSavedViewLogQLBytes bounds a stored query so a pathological client can't
// grow the state store; real LogQL queries are well under 1 KiB.
const maxSavedViewLogQLBytes = 8 << 10

// SavedViewRepo persists RuneSight saved views (named Log Explorer queries).
type SavedViewRepo struct {
	base *BaseRepo[types.SavedView]
}

func NewSavedViewRepo(core store.Store) *SavedViewRepo {
	return &SavedViewRepo{
		base: NewBaseRepo[types.SavedView](core, types.ResourceTypeSavedView),
	}
}

// List returns every saved view, pinned first, then newest-updated first.
func (r *SavedViewRepo) List(ctx context.Context) ([]*types.SavedView, error) {
	views, err := r.base.List(ctx, savedViewNamespaceKey)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].Pinned != views[j].Pinned {
			return views[i].Pinned
		}
		return views[i].UpdatedAt.After(views[j].UpdatedAt)
	})
	return views, nil
}

func (r *SavedViewRepo) Get(ctx context.Context, name string) (*types.SavedView, error) {
	return r.base.Get(ctx, savedViewNamespaceKey, name)
}

// Save upserts a view by name: create when absent, update (preserving
// CreatedAt/CreatedBy) when present. The dashboard's "Save view" is an upsert —
// re-saving an existing name updates the query rather than erroring.
func (r *SavedViewRepo) Save(ctx context.Context, v *types.SavedView) (*types.SavedView, error) {
	if err := r.validate(v); err != nil {
		return nil, err
	}
	now := time.Now()
	cur, err := r.base.Get(ctx, savedViewNamespaceKey, v.Name)
	if err == nil && cur != nil {
		v.ID = cur.ID
		v.CreatedAt = cur.CreatedAt
		if v.CreatedBy == "" {
			v.CreatedBy = cur.CreatedBy
		}
		v.UpdatedAt = now
		if err := r.base.Update(ctx, savedViewNamespaceKey, v.Name, v); err != nil {
			return nil, err
		}
		return v, nil
	}
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	v.CreatedAt = now
	v.UpdatedAt = now
	if err := r.base.Create(ctx, savedViewNamespaceKey, v.Name, v); err != nil {
		return nil, err
	}
	return v, nil
}

func (r *SavedViewRepo) Delete(ctx context.Context, name string) error {
	return r.base.Delete(ctx, savedViewNamespaceKey, name)
}

func (r *SavedViewRepo) validate(v *types.SavedView) error {
	if err := utils.ValidateDNS1123Name(v.Name); err != nil {
		return fmt.Errorf("saved view name validation failed: %w", err)
	}
	if strings.TrimSpace(v.LogQL) == "" {
		return fmt.Errorf("saved view %q: logql is required", v.Name)
	}
	if len(v.LogQL) > maxSavedViewLogQLBytes {
		return fmt.Errorf("saved view %q: logql exceeds %d bytes", v.Name, maxSavedViewLogQLBytes)
	}
	return nil
}
