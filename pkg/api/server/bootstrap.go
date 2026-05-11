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
		}},
		{Name: "readonly", Description: "Read-only", Builtin: true, Rules: []types.PolicyRule{{Resource: "*", Verbs: []string{"get", "list", "watch"}, Namespace: "*"}}},
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
		name := builtins[i].Name
		if _, err := pr.Get(ctx, name); err == nil {
			continue
		}
		err := pr.Create(ctx, &builtins[i])
		if err != nil {
			return err
		}
	}
	return nil
}
