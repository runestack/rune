package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/runestack/rune/pkg/api/authctx"
	"github.com/runestack/rune/pkg/types"
)

// stubAuth grants exactly the verbs in allow, keyed "resource:verb".
type stubAuth struct {
	allow map[string]bool
	err   error
	calls int
}

func (s *stubAuth) Authorize(_ context.Context, _, resource, verb, _ string) (bool, error) {
	s.calls++
	if s.err != nil {
		return false, s.err
	}
	return s.allow[resource+":"+verb], nil
}

func TestServiceRequirements(t *testing.T) {
	unconfined := func(typ string) *types.SecurityContext {
		return &types.SecurityContext{SeccompProfile: &types.SeccompProfile{Type: types.SeccompProfileType(typ)}}
	}
	cases := []struct {
		name string
		svc  *types.Service
		want bool
	}{
		{"nil service", nil, false},
		{"plain", &types.Service{Name: "api"}, false},
		{"empty securityContext", &types.Service{SecurityContext: &types.SecurityContext{}}, false},
		{"privileged main", &types.Service{SecurityContext: &types.SecurityContext{Privileged: true}}, true},
		{"seccomp unconfined main", &types.Service{SecurityContext: unconfined("unconfined")}, true},
		// The k8s-style PascalCase spelling is what users copy-paste; a
		// case-sensitive comparison silently admitted it once already.
		{"seccomp Unconfined main", &types.Service{SecurityContext: unconfined("Unconfined")}, true},
		{"seccomp runtimeDefault main", &types.Service{SecurityContext: unconfined("RuntimeDefault")}, false},
		{"privileged init step", &types.Service{
			InitSteps: []types.InitStep{{Name: "format", SecurityContext: &types.SecurityContext{Privileged: true}}},
		}, true},
		{"unconfined init step", &types.Service{
			InitSteps: []types.InitStep{{Name: "a"}, {Name: "format", SecurityContext: unconfined("Unconfined")}},
		}, true},
		{"plain init steps", &types.Service{InitSteps: []types.InitStep{{Name: "a"}, {Name: "b"}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := len(ServiceRequirements(tc.svc)) > 0
			if got != tc.want {
				t.Fatalf("gate required = %v, want %v", got, tc.want)
			}
			if got {
				r := ServiceRequirements(tc.svc)[0]
				if r.Resource != "services" || r.Verb != "privileged" {
					t.Fatalf("unexpected requirement %+v", r)
				}
			}
		})
	}
}

func TestGateNilAdmitsEverything(t *testing.T) {
	var g *Gate
	svc := &types.Service{SecurityContext: &types.SecurityContext{Privileged: true}}
	if err := g.AdmitService(context.Background(), svc); err != nil {
		t.Fatalf("nil gate must admit, got %v", err)
	}
}

func TestNewGateNilAuthorizerYieldsNilGate(t *testing.T) {
	if NewGate(nil) != nil {
		t.Fatal("NewGate(nil) must not produce a gate that denies everything")
	}
}

func TestGateDeniesWithoutVerb(t *testing.T) {
	g := NewGate(&stubAuth{allow: map[string]bool{}})
	ctx := authctx.WithSubject(context.Background(), "ci")
	svc := &types.Service{Name: "api", Namespace: "app", SecurityContext: &types.SecurityContext{Privileged: true}}

	err := g.AdmitService(ctx, svc)
	if !IsDenied(err) {
		t.Fatalf("expected denial, got %v", err)
	}
	var d *DeniedError
	if !errors.As(err, &d) || d.SubjectID != "ci" || d.Namespace != "app" || d.Verb != "privileged" {
		t.Fatalf("denial lost context: %+v", d)
	}
}

func TestGateAllowsWithVerb(t *testing.T) {
	g := NewGate(&stubAuth{allow: map[string]bool{"services:privileged": true}})
	ctx := authctx.WithSubject(context.Background(), "root")
	svc := &types.Service{Name: "api", Namespace: "app", SecurityContext: &types.SecurityContext{Privileged: true}}
	if err := g.AdmitService(ctx, svc); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
}

// A plain payload must not cost a policy lookup — the gate is on the
// hot path of every service write.
func TestGateSkipsAuthorizerForPlainPayloads(t *testing.T) {
	auth := &stubAuth{allow: map[string]bool{}}
	g := NewGate(auth)
	ctx := authctx.WithSubject(context.Background(), "ci")
	if err := g.AdmitService(ctx, &types.Service{Name: "api"}); err != nil {
		t.Fatalf("plain payload denied: %v", err)
	}
	if auth.calls != 0 {
		t.Fatalf("plain payload consulted the authorizer %d times", auth.calls)
	}
}

// Fail closed: a gate is installed only where authentication stamps a
// subject, so a gated payload arriving without one is a bug, not a
// reason to admit.
func TestGateDeniesWhenSubjectMissing(t *testing.T) {
	g := NewGate(&stubAuth{allow: map[string]bool{"services:privileged": true}})
	svc := &types.Service{Name: "api", SecurityContext: &types.SecurityContext{Privileged: true}}
	if err := g.AdmitService(context.Background(), svc); !IsDenied(err) {
		t.Fatalf("expected denial for a subject-less context, got %v", err)
	}
}

// An authorizer error must never read as "allowed".
func TestGateFailsClosedOnAuthorizerError(t *testing.T) {
	g := NewGate(&stubAuth{err: errors.New("store down")})
	ctx := authctx.WithSubject(context.Background(), "ci")
	svc := &types.Service{Name: "api", SecurityContext: &types.SecurityContext{Privileged: true}}
	err := g.AdmitService(ctx, svc)
	if err == nil {
		t.Fatal("authorizer error must not admit")
	}
	if IsDenied(err) {
		t.Fatal("an authorizer error should surface as an error, not a policy denial")
	}
}

func TestSystemBypass(t *testing.T) {
	g := NewGate(&stubAuth{allow: map[string]bool{}})
	svc := &types.Service{Name: "api", SecurityContext: &types.SecurityContext{Privileged: true}}
	if err := g.AdmitService(WithSystem(context.Background()), svc); err != nil {
		t.Fatalf("system write must bypass admission, got %v", err)
	}
	if IsSystem(context.Background()) {
		t.Fatal("plain context must not read as system")
	}
}
