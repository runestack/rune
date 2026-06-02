package server

import (
	"context"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

// SeedBuiltinNamespaces ensures the built-in namespaces exist (idempotent).
func SeedBuiltinNamespaces(ctx context.Context, st store.Store) error {
	nsRepo := repos.NewNamespaceRepo(st)
	builtins := []types.Namespace{
		{Name: "system"},
		{Name: "default"},
	}
	for i := range builtins {
		name := builtins[i].Name
		if _, err := nsRepo.Get(ctx, name); err == nil {
			continue
		}
		err := nsRepo.CreateBuiltIns(ctx, &builtins[i])
		if err != nil {
			return err
		}
	}
	return nil
}

// SeedBuiltinPolicies ensures the built-in policies exist (idempotent).
func SeedBuiltinPolicies(ctx context.Context, st store.Store) error {
	pr := repos.NewPolicyRepo(st)
	builtins := []types.Policy{
		{Name: "root", Description: "Full access", Builtin: true, Rules: []types.PolicyRule{{Resource: "*", Verbs: []string{"*"}, Namespace: "*"}}},
		{Name: "admin", Description: "Full access", Builtin: true, Rules: []types.PolicyRule{{Resource: "*", Verbs: []string{"*"}, Namespace: "*"}}},
		// readwrite intentionally does NOT include `reveal` on secrets — that
		// requires admin or an explicit policy. It does include the metadata
		// surface (`get`, `list`) which no longer returns plaintext as of dev.33.
		{Name: "readwrite", Description: "Read/write typical ops", Builtin: true, Rules: []types.PolicyRule{
			{Resource: "*", Verbs: []string{"get", "list", "watch", "create", "update", "delete", "scale", "exec"}, Namespace: "*"},
			// Dashboard access (RUNE-200). The wildcard rule above omits the
			// `access` verb, so grant it explicitly on the synthetic `ui`
			// resource. admin/root get it via their "*"/"*" rule; `cast`
			// (CI tokens) deliberately does not.
			{Resource: "ui", Verbs: []string{"access"}, Namespace: "*"},
		}},
		{Name: "readonly", Description: "Read-only", Builtin: true, Rules: []types.PolicyRule{
			{Resource: "*", Verbs: []string{"get", "list", "watch"}, Namespace: "*"},
			{Resource: "ui", Verbs: []string{"access"}, Namespace: "*"}, // dashboard access (RUNE-200)
		}},
		// `cast` is the minimum permission set needed by `rune cast`:
		// it can create/update/scale services and the configmaps/secrets
		// they reference, read instances/logs to confirm rollout, and
		// get/create namespaces (so --create-namespace works). It cannot
		// delete services or exec into them. Designed for CI tokens.
		{Name: "cast", Description: "Cast services (CI deploy)", Builtin: true, Rules: []types.PolicyRule{
			{Resource: "services", Verbs: []string{"get", "list", "create", "update", "scale"}, Namespace: "*"},
			{Resource: "instances", Verbs: []string{"get", "list", "watch"}, Namespace: "*"},
			{Resource: "configmaps", Verbs: []string{"get", "list", "create", "update"}, Namespace: "*"},
			{Resource: "secrets", Verbs: []string{"get", "list", "create", "update"}, Namespace: "*"},
			{Resource: "namespaces", Verbs: []string{"get", "list", "create"}, Namespace: "*"},
			{Resource: "logs", Verbs: []string{"get"}, Namespace: "*"},
			{Resource: "auth", Verbs: []string{"get"}, Namespace: "*"},
		}},
	}
	for i := range builtins {
		desired := builtins[i]
		existing, err := pr.Get(ctx, desired.Name)
		if err != nil {
			// Not present yet — create it.
			if err := pr.Create(ctx, &builtins[i]); err != nil {
				return err
			}
			continue
		}
		// Already present. Reconcile so newly-added built-in rules (e.g. the
		// RUNE-200 `ui:access` grant on readonly/readwrite) reach clusters that
		// bootstrapped before the rule existed — otherwise the idempotent skip
		// would leave those users permanently without the new permission. We
		// only ADD missing (resource, verb) grants to built-in policies; we
		// never remove rules, so operator-tightened custom policies are
		// untouched (they aren't built-ins anyway).
		if !existing.Builtin {
			continue
		}
		if mergePolicyRules(existing, desired.Rules) {
			if err := pr.Update(ctx, existing); err != nil {
				return err
			}
		}
	}
	return nil
}

// mergePolicyRules adds any (resource, namespace, verb) grant from want that is
// missing on have, mutating have in place. It returns true if have changed.
// Verbs are merged into an existing matching (resource, namespace) rule; a
// wholly new (resource, namespace) rule is appended.
func mergePolicyRules(have *types.Policy, want []types.PolicyRule) bool {
	changed := false
	for _, wr := range want {
		idx := -1
		for i := range have.Rules {
			if have.Rules[i].Resource == wr.Resource && have.Rules[i].Namespace == wr.Namespace {
				idx = i
				break
			}
		}
		if idx == -1 {
			have.Rules = append(have.Rules, wr)
			changed = true
			continue
		}
		for _, wv := range wr.Verbs {
			if !containsString(have.Rules[idx].Verbs, wv) {
				have.Rules[idx].Verbs = append(have.Rules[idx].Verbs, wv)
				changed = true
			}
		}
	}
	return changed
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
