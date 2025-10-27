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
		{Name: "readwrite", Description: "Read/write typical ops", Builtin: true, Rules: []types.PolicyRule{{Resource: "*", Verbs: []string{"get", "list", "watch", "create", "update", "delete", "scale", "exec"}, Namespace: "*"}}},
		{Name: "readonly", Description: "Read-only", Builtin: true, Rules: []types.PolicyRule{{Resource: "*", Verbs: []string{"get", "list", "watch"}, Namespace: "*"}}},
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
