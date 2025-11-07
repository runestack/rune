package repos

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

func TestPolicyRepoCRUD(t *testing.T) {
	st := store.NewTestStore()
	r := NewPolicyRepo(st)
	p := &types.Policy{Name: "dev"}
	if err := r.Create(context.Background(), p); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := r.Get(context.Background(), "dev"); err != nil {
		t.Fatalf("get: %v", err)
	}
	p.Description = "updated"
	if err := r.Update(context.Background(), p); err != nil {
		t.Fatalf("update: %v", err)
	}
	ps, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ps) == 0 {
		t.Fatalf("expected at least one policy")
	}
	if err := r.Delete(context.Background(), "dev"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
