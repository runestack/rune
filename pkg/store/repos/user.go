package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

type UserRepo struct{ st store.Store }

func NewUserRepo(st store.Store) *UserRepo { return &UserRepo{st: st} }

func (r *UserRepo) List(ctx context.Context) ([]*types.User, error) {
	var users []*types.User
	if err := r.st.List(ctx, types.ResourceTypeUser, "system", &users); err != nil {
		return nil, err
	}
	return users, nil
}

func (r *UserRepo) Create(ctx context.Context, u *types.User) (*types.User, error) {
	if u.Name == "" {
		return nil, fmt.Errorf("user name required")
	}
	u.ID = uuid.NewString()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	if err := r.st.Create(ctx, types.ResourceTypeUser, "system", u.ID, u); err != nil {
		return nil, err
	}
	return r.Get(ctx, u.ID)
}

func (r *UserRepo) Get(ctx context.Context, id string) (*types.User, error) {
	var u types.User
	if err := r.st.Get(ctx, types.ResourceTypeUser, "system", id, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *UserRepo) GetByName(ctx context.Context, name string) (*types.User, error) {
	// Try to find by name
	users, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		if user.Name == name {
			return user, nil
		}
	}
	return nil, fmt.Errorf("user not found")
}

func (r *UserRepo) GetByNameOrID(ctx context.Context, nameOrID string) (*types.User, error) {
	var u types.User
	if err := r.st.Get(ctx, types.ResourceTypeUser, "system", nameOrID, &u); err != nil {
		if store.IsNotFoundError(err) {
			// Try to find by name
			users, err := r.List(ctx)
			if err != nil {
				return nil, err
			}
			for _, user := range users {
				if user.Name == nameOrID {
					return user, nil
				}
			}
		}
		return nil, err
	}
	return &u, nil
}

func (r *UserRepo) Update(ctx context.Context, u *types.User) error {
	return r.st.Update(ctx, types.ResourceTypeUser, "system", u.ID, u)
}

func (r *UserRepo) Delete(ctx context.Context, id string) error {
	return r.st.Delete(ctx, types.ResourceTypeUser, "system", id)
}
