package orchestrator

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/api/authctx"
	"github.com/runestack/rune/pkg/authz"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

type denyAll struct{}

func (denyAll) Authorize(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, string, string, string, string) (bool, error) {
	return true, nil
}

func newGatedOrchestrator(t *testing.T, auth authz.Authorizer) (*orchestrator, store.Store) {
	t.Helper()
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open store: %v", err)
	}
	o, err := NewOrchestrator(OrchestratorOptions{
		Store:         st,
		Logger:        log.NewTestLogger(),
		RunnerManager: manager.NewTestRunnerManager(nil),
	})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	o.SetAdmission(authz.NewGate(auth))
	return o.(*orchestrator), st
}

func privileged(name string) *types.Service {
	return &types.Service{
		Name: name, Namespace: "default", Image: "nginx",
		SecurityContext: &types.SecurityContext{Privileged: true},
	}
}

// The orchestrator is the choke point BOTH entry points funnel through
// (ServiceService/CreateService and the release applier). Gating it is
// what makes the bypass structurally impossible rather than a rule that
// happens to be listed for the right method names.
func TestCreateService_DeniesPrivilegedWithoutVerb(t *testing.T) {
	o, st := newGatedOrchestrator(t, denyAll{})
	ctx := authctx.WithSubject(context.Background(), "ci")

	err := o.CreateService(ctx, privileged("api"))
	if !authz.IsDenied(err) {
		t.Fatalf("expected denial, got %v", err)
	}
	var got types.Service
	if err := st.Get(context.Background(), types.ResourceTypeService, "default", "api", &got); err == nil {
		t.Fatal("denied service was written to the store")
	}
}

func TestUpdateService_DeniesPrivilegedWithoutVerb(t *testing.T) {
	o, _ := newGatedOrchestrator(t, denyAll{})
	ctx := authctx.WithSubject(context.Background(), "ci")

	// Seed a plain service through an allow-all gate, then try to make
	// it privileged with a token that lacks the verb.
	o.SetAdmission(authz.NewGate(allowAll{}))
	if err := o.CreateService(ctx, &types.Service{Name: "api", Namespace: "default", Image: "nginx"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	o.SetAdmission(authz.NewGate(denyAll{}))

	if err := o.UpdateService(ctx, privileged("api")); !authz.IsDenied(err) {
		t.Fatalf("expected denial on update, got %v", err)
	}
}

func TestCreateService_AllowsPrivilegedWithVerb(t *testing.T) {
	o, _ := newGatedOrchestrator(t, allowAll{})
	ctx := authctx.WithSubject(context.Background(), "root")
	if err := o.CreateService(ctx, privileged("api")); err != nil {
		t.Fatalf("holder of services:privileged was denied: %v", err)
	}
}

func TestCreateService_PlainPayloadUnaffected(t *testing.T) {
	o, _ := newGatedOrchestrator(t, denyAll{})
	ctx := authctx.WithSubject(context.Background(), "ci")
	if err := o.CreateService(ctx, &types.Service{Name: "api", Namespace: "default", Image: "nginx"}); err != nil {
		t.Fatalf("plain create denied: %v", err)
	}
}

// No gate installed (auth disabled, in-process tests) means no change
// in behaviour.
func TestCreateService_NoGateAdmits(t *testing.T) {
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open store: %v", err)
	}
	o, err := NewOrchestrator(OrchestratorOptions{
		Store:         st,
		Logger:        log.NewTestLogger(),
		RunnerManager: manager.NewTestRunnerManager(nil),
	})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	if err := o.CreateService(context.Background(), privileged("api")); err != nil {
		t.Fatalf("ungated orchestrator denied: %v", err)
	}
}
