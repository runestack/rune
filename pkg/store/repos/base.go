package repos

import (
	"context"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// BaseRepo provides common CRUD over the core store for a specific resource type.
// T is the typed payload struct (e.g., *types.Service, *types.Secret).
type BaseRepo[T any] struct {
	core         store.Store
	resourceType types.ResourceType
}

func NewBaseRepo[T any](core store.Store, rt types.ResourceType) *BaseRepo[T] {
	return &BaseRepo[T]{core: core, resourceType: rt}
}

func (r *BaseRepo[T]) Create(ctx context.Context, namespace, name string, obj *T) error {
	return r.core.Create(ctx, r.resourceType, namespace, name, obj)
}

func (r *BaseRepo[T]) Get(ctx context.Context, namespace, name string) (*T, error) {
	var out T
	if err := r.core.Get(ctx, r.resourceType, namespace, name, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *BaseRepo[T]) Update(ctx context.Context, namespace, name string, obj *T, opts ...store.UpdateOption) error {
	return r.core.Update(ctx, r.resourceType, namespace, name, obj, opts...)
}

func (r *BaseRepo[T]) Delete(ctx context.Context, namespace, name string) error {
	return r.core.Delete(ctx, r.resourceType, namespace, name)
}

// Ref-based helpers
func (r *BaseRepo[T]) CreateRef(ctx context.Context, ref string, obj *T) error {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return err
	}
	return r.Create(ctx, pr.Namespace, pr.Name, obj)
}

func (r *BaseRepo[T]) GetRef(ctx context.Context, ref string) (*T, error) {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, pr.Namespace, pr.Name)
}

func (r *BaseRepo[T]) UpdateRef(ctx context.Context, ref string, obj *T, opts ...store.UpdateOption) error {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return err
	}
	return r.Update(ctx, pr.Namespace, pr.Name, obj, opts...)
}

func (r *BaseRepo[T]) DeleteRef(ctx context.Context, ref string) error {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return err
	}
	return r.Delete(ctx, pr.Namespace, pr.Name)
}

// List returns typed list within a namespace
func (r *BaseRepo[T]) List(ctx context.Context, namespace string) ([]*T, error) {
	var items []T
	if err := r.core.List(ctx, r.resourceType, namespace, &items); err != nil {
		return nil, err
	}
	out := make([]*T, 0, len(items))
	for i := range items {
		item := items[i]
		out = append(out, &item)
	}
	return out, nil
}

// Watch proxies to the core store
func (r *BaseRepo[T]) Watch(ctx context.Context, namespace string) (<-chan store.WatchEvent, error) {
	return r.core.Watch(ctx, r.resourceType, namespace)
}

func (r *BaseRepo[T]) GetHistory(ctx context.Context, ref string) ([]store.HistoricalVersion, error) {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return nil, err
	}
	return r.core.GetHistory(ctx, r.resourceType, pr.Namespace, pr.Name)
}

func (r *BaseRepo[T]) GetVersion(ctx context.Context, ref string, version string) (*T, error) {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return nil, err
	}
	res, err := r.core.GetVersion(ctx, r.resourceType, pr.Namespace, pr.Name, version)
	if err != nil {
		return nil, err
	}
	if typed, ok := res.(*T); ok && typed != nil {
		return typed, nil
	}
	var out T
	return &out, nil
}

func (r *BaseRepo[T]) Core() store.Store { return r.core }
