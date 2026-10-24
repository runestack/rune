package server

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/store"
)

// A cross-namespace volume claim or snapshot restore needs volumes:reveal /
// snapshots:reveal (authz.ReleaseChangeRequirement). Like secrets:reveal,
// these must be held by admin and root only, via their wildcard rule, and by
// no lesser builtin — otherwise the cross-namespace gate is toothless.
func TestSeedBuiltinPolicies_RevealVerbsAdminOnly(t *testing.T) {
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := SeedBuiltinPolicies(context.Background(), st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, res := range []string{"volumes", "snapshots", "secrets"} {
		for _, p := range []string{"root", "admin"} {
			if !policyGrants(t, st, p, res, "reveal") {
				t.Errorf("%s should grant %s:reveal", p, res)
			}
		}
		for _, p := range []string{"readwrite", "readonly", "cast"} {
			if policyGrants(t, st, p, res, "reveal") {
				t.Errorf("%s must NOT grant %s:reveal", p, res)
			}
		}
	}
}
