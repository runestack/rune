package service

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/api/authctx"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/authz"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type denyAllAuthz struct{}

func (denyAllAuthz) Authorize(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

// The privileged gate moved off the RBAC interceptor table and onto the
// orchestrator choke point. That is only a safe move if the gRPC entry
// point — the one the interceptor used to cover — still denies. This is
// the other half of the pair with
// releasectl.TestCast_DeniesPrivilegedWithoutVerb.
func TestServiceService_CreateDeniesPrivilegedWithoutVerb(t *testing.T) {
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open store: %v", err)
	}
	orch, err := orchestrator.NewOrchestrator(orchestrator.OrchestratorOptions{
		Store:         st,
		Logger:        log.NewTestLogger(),
		RunnerManager: manager.NewTestRunnerManager(nil),
	})
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	orch.SetAdmission(authz.NewGate(denyAllAuthz{}))

	svc := NewServiceService(st, orch, nil, log.NewTestLogger())
	ctx := authctx.WithSubject(context.Background(), "ci")

	_, err = svc.CreateService(ctx, &generated.CreateServiceRequest{
		Service: &generated.Service{
			Name: "api", Namespace: "default", Image: "nginx", Scale: 1,
			SecurityContext: &generated.SecurityContext{Privileged: true},
		},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
	var got types.Service
	if err := st.Get(context.Background(), types.ResourceTypeService, "default", "api", &got); err == nil {
		t.Fatal("denied service was written to the store")
	}
}
