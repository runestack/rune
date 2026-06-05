package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

type ConfigmapRepo struct {
	base         *BaseRepo[types.Configmap]
	configLimits store.Limits
}

type ConfigOption func(*ConfigmapRepo)

func NewConfigRepo(core store.Store, opts ...ConfigOption) *ConfigmapRepo {
	repo := &ConfigmapRepo{
		base: NewBaseRepo[types.Configmap](core, types.ResourceTypeConfigmap),
	}
	repo.configLimits = core.GetOpts().ConfigLimits
	for _, opt := range opts {
		opt(repo)
	}
	return repo
}

func WithConfigLimits(limits store.Limits) ConfigOption {
	return func(r *ConfigmapRepo) {
		r.configLimits = limits
	}
}

// List returns configs in a namespace
func (r *ConfigmapRepo) List(ctx context.Context, namespace string) ([]*types.Configmap, error) {
	return r.base.List(ctx, namespace)
}

func (r *ConfigmapRepo) Create(ctx context.Context, ref string, c *types.Configmap) error {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return err
	}

	if err := utils.ValidateDNS1123Name(c.Name); err != nil {
		return fmt.Errorf("configmap name validation failed: %w", err)
	}

	if err := r.validateConfigData(c.Data); err != nil {
		return err
	}

	if c.ID == "" {
		c.ID = uuid.NewString()
	}
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now

	name := c.Name
	if name == "" {
		name = pr.Name
	}
	return r.base.Create(ctx, pr.Namespace, name, c)
}

func (r *ConfigmapRepo) Get(ctx context.Context, namespace, name string) (*types.Configmap, error) {
	return r.base.Get(ctx, namespace, name)
}

func (r *ConfigmapRepo) Update(ctx context.Context, namespace, name string, c *types.Configmap, opts ...store.UpdateOption) error {
	if err := r.validateConfigData(c.Data); err != nil {
		return err
	}

	// Fetch current to compute next version and preserve creation time
	cur, err := r.base.Get(ctx, namespace, name)
	if err != nil {
		return err
	}

	c.CreatedAt = cur.CreatedAt
	c.UpdatedAt = time.Now()
	c.Version = cur.Version + 1
	return r.base.Update(ctx, namespace, name, c, opts...)
}

func (r *ConfigmapRepo) Delete(ctx context.Context, namespace, name string) error {
	return r.base.Delete(ctx, namespace, name)
}

// ListVersions returns every historical version of a configmap, newest first.
// The core store keeps each Update as a distinct version row (the same
// machinery that backs `rune secret versions`), so this just reads that history
// — configmaps are plaintext, so no decryption is involved.
func (r *ConfigmapRepo) ListVersions(ctx context.Context, namespace, name string) ([]*types.Configmap, error) {
	hist, err := r.base.core.GetHistory(ctx, types.ResourceTypeConfigmap, namespace, name)
	if err != nil {
		return nil, err
	}
	out := make([]*types.Configmap, 0, len(hist))
	for _, h := range hist {
		c, err := historyToConfigmap(h.Resource)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	// Store order is newest-first by storage timestamp; ensure strict descending
	// Version ordering for stable callers.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Version > out[i].Version {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// GetVersionN returns a specific historical version by its user-facing integer
// Version (not the opaque store version ID).
func (r *ConfigmapRepo) GetVersionN(ctx context.Context, namespace, name string, version int) (*types.Configmap, error) {
	versions, err := r.ListVersions(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	for _, c := range versions {
		if c.Version == version {
			return c, nil
		}
	}
	return nil, fmt.Errorf("version %d of configmap %s/%s not found", version, namespace, name)
}

// Rollback rewrites the configmap to the data of a prior version, producing a
// NEW version (head+1). History is retained.
func (r *ConfigmapRepo) Rollback(ctx context.Context, namespace, name string, toVersion int, opts ...store.UpdateOption) (*types.Configmap, error) {
	prior, err := r.GetVersionN(ctx, namespace, name, toVersion)
	if err != nil {
		return nil, err
	}
	desired := &types.Configmap{Name: name, Namespace: namespace, Data: prior.Data}
	if err := r.Update(ctx, namespace, name, desired, opts...); err != nil {
		return nil, err
	}
	return r.Get(ctx, namespace, name)
}

// historyToConfigmap re-marshals an interface{} returned by Store.GetHistory
// into a *types.Configmap. GetHistory hands back map[string]interface{} after
// json.Unmarshal, so round-tripping through json recovers the typed shape.
func historyToConfigmap(raw interface{}) (*types.Configmap, error) {
	switch v := raw.(type) {
	case types.Configmap:
		c := v
		return &c, nil
	case *types.Configmap:
		if v == nil {
			return nil, fmt.Errorf("nil configmap in history")
		}
		return v, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("re-marshal history entry: %w", err)
	}
	var c types.Configmap
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("decode history entry: %w", err)
	}
	return &c, nil
}

func (r *ConfigmapRepo) validateConfigData(data map[string]string) error {
	var total int
	for k, v := range data {
		if len(k) > r.configLimits.MaxKeyNameLength {
			return fmt.Errorf("config key name too long: %s", k)
		}
		total += len(v)
		if total > r.configLimits.MaxObjectBytes {
			return fmt.Errorf("config data exceeds 1MiB limit")
		}
	}
	return nil
}
