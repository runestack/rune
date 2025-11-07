package repos

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

type PolicyRepo struct{ st store.Store }

func NewPolicyRepo(st store.Store) *PolicyRepo { return &PolicyRepo{st: st} }

func (r *PolicyRepo) Create(ctx context.Context, p *types.Policy) error {
	if err := utils.ValidateDNS1123Name(p.Name); err != nil {
		return fmt.Errorf("policy name validation failed: %w", err)
	}
	p.ID = uuid.NewString()
	return r.st.Create(ctx, types.ResourceTypePolicy, "system", p.Name, p)
}

func (r *PolicyRepo) Update(ctx context.Context, p *types.Policy) error {
	return r.st.Update(ctx, types.ResourceTypePolicy, "system", p.Name, p)
}

func (r *PolicyRepo) Get(ctx context.Context, name string) (*types.Policy, error) {
	var p types.Policy
	if err := r.st.Get(ctx, types.ResourceTypePolicy, "system", name, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PolicyRepo) Delete(ctx context.Context, name string) error {
	return r.st.Delete(ctx, types.ResourceTypePolicy, "system", name)
}

func (r *PolicyRepo) List(ctx context.Context) ([]types.Policy, error) {
	var ps []types.Policy
	if err := r.st.List(ctx, types.ResourceTypePolicy, "system", &ps); err != nil {
		return nil, err
	}
	return ps, nil
}
