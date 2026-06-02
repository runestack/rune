package server

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

// policyGrants reports whether the named policy allows (resource, verb).
func policyGrants(t *testing.T, st store.Store, policy, resource, verb string) bool {
	t.Helper()
	p, err := repos.NewPolicyRepo(st).Get(context.Background(), policy)
	if err != nil {
		t.Fatalf("get policy %s: %v", policy, err)
	}
	for _, r := range p.Rules {
		if r.Resource != "*" && r.Resource != resource {
			continue
		}
		for _, v := range r.Verbs {
			if v == "*" || v == verb {
				return true
			}
		}
	}
	return false
}

// TestSeedBuiltinPolicies_UIAccessFreshBootstrap verifies the ui:access grant
// lands on a clean bootstrap for the intended roles, and is withheld from cast.
func TestSeedBuiltinPolicies_UIAccessFreshBootstrap(t *testing.T) {
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := SeedBuiltinPolicies(context.Background(), st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, p := range []string{"root", "admin", "readwrite", "readonly"} {
		if !policyGrants(t, st, p, "ui", "access") {
			t.Errorf("policy %q should grant ui:access", p)
		}
	}
	if policyGrants(t, st, "cast", "ui", "access") {
		t.Errorf("policy cast must NOT grant ui:access")
	}
}

// TestSeedBuiltinPolicies_UIAccessReconcilesUpgrade simulates a cluster that
// bootstrapped before RUNE-200: readonly/readwrite exist WITHOUT the ui:access
// rule. Re-seeding must reconcile the grant onto them (the idempotent skip
// would otherwise lock those users out of the dashboard).
func TestSeedBuiltinPolicies_UIAccessReconcilesUpgrade(t *testing.T) {
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	pr := repos.NewPolicyRepo(st)

	// Pre-existing pre-RUNE-200 builtins: no ui rule.
	old := []types.Policy{
		{Name: "readonly", Builtin: true, Rules: []types.PolicyRule{
			{Resource: "*", Verbs: []string{"get", "list", "watch"}, Namespace: "*"},
		}},
		{Name: "readwrite", Builtin: true, Rules: []types.PolicyRule{
			{Resource: "*", Verbs: []string{"get", "list", "watch", "create", "update", "delete", "scale", "exec"}, Namespace: "*"},
		}},
	}
	for i := range old {
		if err := pr.Create(ctx, &old[i]); err != nil {
			t.Fatalf("seed old policy: %v", err)
		}
	}
	if policyGrants(t, st, "readonly", "ui", "access") {
		t.Fatal("precondition: readonly should NOT have ui:access before reconcile")
	}

	if err := SeedBuiltinPolicies(ctx, st); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	if !policyGrants(t, st, "readonly", "ui", "access") {
		t.Errorf("readonly should have ui:access after reconcile")
	}
	if !policyGrants(t, st, "readwrite", "ui", "access") {
		t.Errorf("readwrite should have ui:access after reconcile")
	}
	// Reconcile must not drop the original verbs.
	if !policyGrants(t, st, "readonly", "services", "get") {
		t.Errorf("reconcile dropped existing readonly grants")
	}
}

// TestSeedBuiltinPolicies_DoesNotTouchNonBuiltin ensures a custom (non-builtin)
// policy that happens to share a builtin name is left alone.
func TestSeedBuiltinPolicies_DoesNotTouchNonBuiltin(t *testing.T) {
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	pr := repos.NewPolicyRepo(st)
	// Operator-defined policy named "readonly" but NOT builtin.
	custom := &types.Policy{Name: "readonly", Builtin: false, Rules: []types.PolicyRule{
		{Resource: "services", Verbs: []string{"get"}, Namespace: "prod"},
	}}
	if err := pr.Create(ctx, custom); err != nil {
		t.Fatalf("create custom: %v", err)
	}
	if err := SeedBuiltinPolicies(ctx, st); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, _ := pr.Get(ctx, "readonly")
	if len(got.Rules) != 1 {
		t.Errorf("non-builtin policy was modified by seed: %+v", got.Rules)
	}
}
