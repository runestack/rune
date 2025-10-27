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
