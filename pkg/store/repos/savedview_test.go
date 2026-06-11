package repos

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

func TestSavedViewRepo_SaveIsUpsert(t *testing.T) {
	r := NewSavedViewRepo(store.NewTestStore())
	ctx := context.Background()

	v1, err := r.Save(ctx, &types.SavedView{Name: "payment-errors", LogQL: `{service="payments", level="error"}`, Range: "1h", CreatedBy: "ore"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if v1.ID == "" || v1.CreatedAt.IsZero() {
		t.Fatalf("create did not stamp identity: %+v", v1)
	}

	// Re-saving the same name updates the query and preserves identity.
	time.Sleep(2 * time.Millisecond)
	v2, err := r.Save(ctx, &types.SavedView{Name: "payment-errors", LogQL: `{service="payments"} |= "boom"`, Range: "24h"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if v2.ID != v1.ID {
		t.Errorf("upsert minted a new ID: %s != %s", v2.ID, v1.ID)
	}
	if !v2.CreatedAt.Equal(v1.CreatedAt) || v2.CreatedBy != "ore" {
		t.Errorf("upsert lost creation identity: %+v", v2)
	}
	if !v2.UpdatedAt.After(v1.UpdatedAt) {
		t.Errorf("UpdatedAt not advanced")
	}

	got, err := r.Get(ctx, "payment-errors")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LogQL != `{service="payments"} |= "boom"` || got.Range != "24h" {
		t.Fatalf("update not persisted: %+v", got)
	}
}

func TestSavedViewRepo_ListPinnedFirstThenNewest(t *testing.T) {
	r := NewSavedViewRepo(store.NewTestStore())
	ctx := context.Background()

	mustSave := func(v *types.SavedView) {
		t.Helper()
		if _, err := r.Save(ctx, v); err != nil {
			t.Fatalf("save %s: %v", v.Name, err)
		}
		time.Sleep(2 * time.Millisecond) // distinct UpdatedAt ordering
	}
	mustSave(&types.SavedView{Name: "oldest", LogQL: `{service="a"}`})
	mustSave(&types.SavedView{Name: "pinned-one", LogQL: `{service="b"}`, Pinned: true})
	mustSave(&types.SavedView{Name: "newest", LogQL: `{service="c"}`})

	views, err := r.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 3 {
		t.Fatalf("want 3 views, got %d", len(views))
	}
	if views[0].Name != "pinned-one" {
		t.Errorf("pinned view must sort first, got %s", views[0].Name)
	}
	if views[1].Name != "newest" || views[2].Name != "oldest" {
		t.Errorf("unpinned views must sort newest-updated first: %s, %s", views[1].Name, views[2].Name)
	}
}

func TestSavedViewRepo_DeleteAndValidation(t *testing.T) {
	r := NewSavedViewRepo(store.NewTestStore())
	ctx := context.Background()

	if _, err := r.Save(ctx, &types.SavedView{Name: "tmp", LogQL: `{service="x"}`}); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "tmp"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.Get(ctx, "tmp"); err == nil {
		t.Fatal("get after delete should fail")
	}

	// Validation: bad name, empty logql, oversize logql.
	if _, err := r.Save(ctx, &types.SavedView{Name: "Not A DNS Name", LogQL: "{}"}); err == nil {
		t.Error("want name validation error")
	}
	if _, err := r.Save(ctx, &types.SavedView{Name: "no-query", LogQL: "  "}); err == nil {
		t.Error("want empty-logql error")
	}
	if _, err := r.Save(ctx, &types.SavedView{Name: "huge", LogQL: strings.Repeat("x", maxSavedViewLogQLBytes+1)}); err == nil {
		t.Error("want oversize-logql error")
	}
}
