package server

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/store"
)

// The cast role writes every kind a release carries, but deletes none:
// release writes are checked against the same verbs as direct RPCs, so a
// delete here would let a CI token prune any service it can adopt.
func TestSeedBuiltinPolicies_CastWritesButNeverDeletes(t *testing.T) {
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := SeedBuiltinPolicies(context.Background(), st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, res := range []string{"services", "secrets", "configmaps", "volumes"} {
		for _, verb := range []string{"create", "update"} {
			if !policyGrants(t, st, "cast", res, verb) {
				t.Errorf("cast should grant %s:%s", res, verb)
			}
		}
		if policyGrants(t, st, "cast", res, "delete") {
			t.Errorf("cast must NOT grant %s:delete", res)
		}
	}
}
