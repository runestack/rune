package releasectl

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/api/authctx"
	"github.com/runestack/rune/pkg/authz"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/types"
)

// denyAll refuses every extra verb, standing in for a `cast`-scoped CI
// token: it can create releases and services but holds no
// services.privileged grant.
type denyAll struct{}

func (denyAll) Authorize(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, string, string, string, string) (bool, error) {
	return true, nil
}

func privSpec(detach bool) (release.ReleaseSpec, Payloads) {
	ref := svcRef("default", "web")
	return release.ReleaseSpec{
			Name:      "app",
			Namespace: "default",
			Detach:    detach,
			Resources: []release.DesiredResource{{Ref: ref}},
		}, Payloads{
			Services: map[string]*types.Service{
				ref.Key(): {
					Name: "web", Namespace: "default",
					SecurityContext: &types.SecurityContext{Privileged: true},
				},
			},
		}
}

// TestCast_DeniesPrivilegedWithoutVerb pins the cast path to the
// services:privileged gate. A cast reaches the orchestrator in-process
// through ReleaseService, so a gate keyed on ServiceService method names
// does not cover it.
func TestCast_DeniesPrivilegedWithoutVerb(t *testing.T) {
	c, _ := newTestController(t)
	c.SetAdmission(authz.NewGate(denyAll{}))
	ctx := authctx.WithSubject(context.Background(), "ci")

	spec, payloads := privSpec(false)
	_, _, err := c.Cast(ctx, spec, payloads)
	if !authz.IsDenied(err) {
		t.Fatalf("expected a privileged denial, got %v", err)
	}

	// Denial must happen before anything is applied — a half-applied
	// release that then needs rolling back is not an acceptable outcome
	// for an authorization failure.
	if _, err := c.releases.GetByName(context.Background(), "default", "app"); err == nil {
		t.Fatal("a denied cast recorded a release")
	}
}

// The --detach path hands the apply to a goroutine with a fresh
// context, dropping the subject. Admission must therefore complete
// synchronously, inside Cast, or the denial would land unobserved in
// the background (or not at all).
func TestCast_DeniesPrivilegedOnDetachPathSynchronously(t *testing.T) {
	c, _ := newTestController(t)
	c.SetAdmission(authz.NewGate(denyAll{}))
	ctx := authctx.WithSubject(context.Background(), "ci")

	spec, payloads := privSpec(true)
	if _, _, err := c.Cast(ctx, spec, payloads); !authz.IsDenied(err) {
		t.Fatalf("expected a synchronous privileged denial on the detach path, got %v", err)
	}
}

func TestCast_AllowsPrivilegedWithVerb(t *testing.T) {
	c, _ := newTestController(t)
	c.SetAdmission(authz.NewGate(allowAll{}))
	ctx := authctx.WithSubject(context.Background(), "root")

	spec, payloads := privSpec(false)
	if _, _, err := c.Cast(ctx, spec, payloads); err != nil {
		t.Fatalf("holder of services:privileged was denied: %v", err)
	}
}

// A plain release must be unaffected by the gate.
func TestCast_PlainPayloadUnaffected(t *testing.T) {
	c, _ := newTestController(t)
	c.SetAdmission(authz.NewGate(denyAll{}))
	ctx := authctx.WithSubject(context.Background(), "ci")

	ref := svcRef("default", "web")
	spec := release.ReleaseSpec{
		Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: ref}},
	}
	payloads := Payloads{Services: map[string]*types.Service{
		ref.Key(): {Name: "web", Namespace: "default"},
	}}
	if _, _, err := c.Cast(ctx, spec, payloads); err != nil {
		t.Fatalf("plain cast denied: %v", err)
	}
}

// An init step's securityContext is gated exactly like the main
// container's — the two must never drift.
func TestCast_DeniesPrivilegedInitStep(t *testing.T) {
	c, _ := newTestController(t)
	c.SetAdmission(authz.NewGate(denyAll{}))
	ctx := authctx.WithSubject(context.Background(), "ci")

	ref := svcRef("default", "tb")
	spec := release.ReleaseSpec{
		Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: ref}},
	}
	payloads := Payloads{Services: map[string]*types.Service{
		ref.Key(): {
			Name: "tb", Namespace: "default",
			InitSteps: []types.InitStep{{
				Name: "format",
				SecurityContext: &types.SecurityContext{
					SeccompProfile: &types.SeccompProfile{Type: "Unconfined"},
				},
			}},
		},
	}}
	if _, _, err := c.Cast(ctx, spec, payloads); !authz.IsDenied(err) {
		t.Fatalf("expected an init-step denial, got %v", err)
	}
}
