package repos

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// newTestSecretRepo builds a SecretRepo over an in-memory test store with a
// fixed KEK and generous limits.
func newTestSecretRepo() *SecretRepo {
	st := store.NewTestStore()
	kek := make([]byte, 32) // deterministic zero key — fine for tests
	return NewSecretRepo(st,
		WithKEKBytes(kek),
		WithSecretLimits(store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256}),
	)
}

// The OwnedBy stamp is plaintext metadata on StoredSecret; it must survive the
// encrypt → store → decrypt round-trip so runeset releases own secrets the same
// way they own configmaps (regression: it used to be dropped, so secrets looked
// "unmanaged" to drift/adoption checks).
func TestSecretRepo_OwnedByRoundTrips(t *testing.T) {
	repo := newTestSecretRepo()
	ctx := context.Background()

	s := &types.Secret{
		Name:      "creds",
		Namespace: "default",
		Data:      map[string]string{"username": "admin"},
		OwnedBy:   &types.OwnedBy{Release: "demo", Revision: 2, Manager: types.ManagerRuneset},
	}
	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.Get(ctx, "default", "creds")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OwnedBy == nil {
		t.Fatal("OwnedBy was dropped on the encrypt/store/decrypt round-trip")
	}
	if got.OwnedBy.Release != "demo" || got.OwnedBy.Revision != 2 || got.OwnedBy.Manager != types.ManagerRuneset {
		t.Errorf("OwnedBy mismatch: got %+v", got.OwnedBy)
	}
}

// An unmanaged secret (no stamp) must round-trip as nil, not an empty struct,
// so the planner can distinguish "unowned" from "owned".
func TestSecretRepo_NoOwnerStaysNil(t *testing.T) {
	repo := newTestSecretRepo()
	ctx := context.Background()
	if err := repo.Create(ctx, &types.Secret{
		Name: "plain", Namespace: "default", Data: map[string]string{"k": "v"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.Get(ctx, "default", "plain")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OwnedBy != nil {
		t.Errorf("want nil OwnedBy for unmanaged secret, got %+v", got.OwnedBy)
	}
}
