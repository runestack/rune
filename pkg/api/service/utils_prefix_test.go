package service

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

func seedInst(t *testing.T, st *store.TestStore, id, name string, status types.InstanceStatus, failedAt *time.Time) {
	t.Helper()
	in := &types.Instance{
		ID: id, Namespace: "default", Name: name,
		ServiceName: "svc", Status: status, FailedAt: failedAt,
	}
	if err := st.Create(context.Background(), types.ResourceTypeInstance, "default", id, in); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// TestResolveResourceTarget_IDPrefix covers the git/docker-style short-id
// resolution added for #157 (the table now prints an 8-hex prefix).
func TestResolveResourceTarget_IDPrefix(t *testing.T) {
	st := store.NewTestStore()
	// web-a and web-d share the 6-hex prefix "0f238d"; "0f238d0c" is unique.
	seedInst(t, st, "0f238d0c-cd8e-4d37-922f-b554965a4ecd", "web-a", types.InstanceStatusRunning, nil)
	seedInst(t, st, "0f238d99-1111-2222-3333-444455556666", "web-d", types.InstanceStatusRunning, nil)
	seedInst(t, st, "abcdef12-9999-8888-7777-666655554444", "web-c", types.InstanceStatusRunning, nil)
	ctx := context.Background()

	// Unique 8-hex prefix (what the table prints) resolves to that instance.
	tgt, err := resolveResourceTarget(ctx, st, "0f238d0c", "default")
	if err != nil {
		t.Fatalf("unique prefix should resolve: %v", err)
	}
	if in, _ := tgt.GetInstance(); in == nil || in.Name != "web-a" {
		t.Fatalf("resolved wrong instance: %+v", tgt.Resource)
	}

	// "0f238d" (6 hex) matches web-a AND web-d → ambiguous error, never a
	// silent wrong pick.
	if _, err := resolveResourceTarget(ctx, st, "0f238d", "default"); err == nil {
		t.Fatal("expected ambiguous prefix to error")
	}

	// A hex prefix matching nothing errors (falls through to not-found).
	if _, err := resolveResourceTarget(ctx, st, "0f2345", "default"); err == nil {
		t.Fatal("a hex prefix matching nothing should error")
	}

	// Non-hex / too-short args must NOT trigger prefix matching.
	if _, err := resolveResourceTarget(ctx, st, "0f2z9a", "default"); err == nil {
		t.Fatal("non-hex arg should not resolve as a prefix")
	}

	// Exact full UUID still works (fast path).
	if _, err := resolveResourceTarget(ctx, st, "abcdef12-9999-8888-7777-666655554444", "default"); err != nil {
		t.Fatalf("full uuid should resolve: %v", err)
	}
}

func TestIsHexIDPrefix(t *testing.T) {
	cases := map[string]bool{
		"0f238d0c": true,
		"abcdef":   true,
		"0f2":      false, // too short
		"0f238d0z": false, // non-hex
		"web-a":    false,
		"ABCDEF12": false, // uppercase not accepted (UUIDs are lowercase)
		"":         false,
	}
	for in, want := range cases {
		if got := isHexIDPrefix(in); got != want {
			t.Errorf("isHexIDPrefix(%q) = %v, want %v", in, got, want)
		}
	}
}
