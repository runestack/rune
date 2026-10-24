package releasectl

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/api/authctx"
	"github.com/runestack/rune/pkg/authz"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/types"
)

// castScoped stands in for a `cast`-scoped CI token: it may read, create
// and update every kind a release writes, in namespace "default" only, and
// holds neither delete nor services.privileged.
type castScoped struct{}

func (castScoped) Authorize(_ context.Context, _, resource, verb, ns string) (bool, error) {
	if ns != "default" {
		return false, nil
	}
	switch resource {
	case "services", "secrets", "configmaps", "volumes":
		return verb == "get" || verb == "create" || verb == "update", nil
	}
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
	c.SetAdmission(authz.NewGate(castScoped{}))
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
	c.SetAdmission(authz.NewGate(castScoped{}))
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
	c.SetAdmission(authz.NewGate(castScoped{}))
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
	c.SetAdmission(authz.NewGate(castScoped{}))
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

// Adopting a service and then casting the release without it would prune
// it. A cast-scoped token holds no delete, so the prune is denied before
// anything is recorded or applied, and the service survives.
func TestCast_PruneNeedsDelete(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	web, victim := svcRef("default", "web"), svcRef("default", "victim")
	payloads := Payloads{Services: map[string]*types.Service{
		web.Key():    {Name: "web", Namespace: "default"},
		victim.Key(): {Name: "victim", Namespace: "default"},
	}}
	if _, _, err := c.Cast(ctx, release.ReleaseSpec{
		Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: web}, {Ref: victim}},
	}, payloads); err != nil {
		t.Fatalf("seed cast: %v", err)
	}
	before, err := c.releases.GetByName(ctx, "default", "app")
	if err != nil {
		t.Fatalf("get release: %v", err)
	}

	c.SetAdmission(authz.NewGate(castScoped{}))
	ci := authctx.WithSubject(ctx, "ci")
	_, _, err = c.Cast(ci, release.ReleaseSpec{
		Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: web}},
	}, payloads)
	if !authz.IsDenied(err) {
		t.Fatalf("expected a delete denial, got %v", err)
	}
	if _, err := c.orch.GetService(ctx, "default", "victim"); err != nil {
		t.Fatalf("victim was deleted: %v", err)
	}
	after, _ := c.releases.GetByName(ctx, "default", "app")
	if after.Revision != before.Revision || after.Status != before.Status {
		t.Errorf("denied cast changed the record: rev %d→%d, status %q→%q",
			before.Revision, after.Revision, before.Status, after.Status)
	}
}

// Uninstall deletes everything the release owns, so it needs delete too,
// and a denial leaves the release untouched rather than half-uninstalled.
func TestUninstall_NeedsDelete(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	web := svcRef("default", "web")
	if _, _, err := c.Cast(ctx, release.ReleaseSpec{
		Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: web}},
	}, Payloads{Services: map[string]*types.Service{web.Key(): {Name: "web", Namespace: "default"}}}); err != nil {
		t.Fatalf("seed cast: %v", err)
	}

	c.SetAdmission(authz.NewGate(castScoped{}))
	err := c.Uninstall(authctx.WithSubject(ctx, "ci"), "default", "app", UninstallOptions{})
	if !authz.IsDenied(err) {
		t.Fatalf("expected a delete denial, got %v", err)
	}
	if _, err := c.orch.GetService(ctx, "default", "web"); err != nil {
		t.Fatalf("service was deleted: %v", err)
	}
	rel, _ := c.releases.GetByName(ctx, "default", "app")
	if rel.Status != types.ReleaseStatusDeployed {
		t.Errorf("denied uninstall left status %q", rel.Status)
	}
}

// A namespace-pinned token cannot write outside its namespace by naming
// another one on the resource: each change is checked in its own.
func TestCast_ChecksEachResourcesNamespace(t *testing.T) {
	c, _ := newTestController(t)
	c.SetAdmission(authz.NewGate(castScoped{}))
	ctx := authctx.WithSubject(context.Background(), "ci")

	other := svcRef("prod", "web")
	_, _, err := c.Cast(ctx, release.ReleaseSpec{
		Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: other}},
	}, Payloads{Services: map[string]*types.Service{other.Key(): {Name: "web", Namespace: "prod"}}})
	if !authz.IsDenied(err) {
		t.Fatalf("expected a denial in prod, got %v", err)
	}
	if _, err := c.orch.GetService(context.Background(), "prod", "web"); err == nil {
		t.Fatal("service created outside the token's namespace")
	}
}

// Volumes are written through the release path like any other kind; the
// cast role's volume grant is what lets a plain cast carry one.
func TestCast_VolumeNeedsVolumeVerb(t *testing.T) {
	vol := types.OwnerRef{ResourceType: types.ResourceTypeVolume, Namespace: "default", Name: "data"}
	spec := release.ReleaseSpec{
		Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: vol}},
	}
	payloads := Payloads{Volumes: map[string]*types.Volume{vol.Key(): {Name: "data", Namespace: "default", StorageClassName: "local", Size: "1Gi"}}}
	ctx := authctx.WithSubject(context.Background(), "ci")

	c, _ := newTestController(t)
	c.SetAdmission(authz.NewGate(noVolumes{}))
	if _, _, err := c.Cast(ctx, spec, payloads); !authz.IsDenied(err) {
		t.Fatalf("expected a volumes denial, got %v", err)
	}

	c, _ = newTestController(t)
	c.SetAdmission(authz.NewGate(castScoped{}))
	if _, _, err := c.Cast(ctx, spec, payloads); err != nil {
		t.Fatalf("cast-scoped volume cast denied: %v", err)
	}
}

// noVolumes is castScoped as it was before volumes were granted.
type noVolumes struct{}

func (noVolumes) Authorize(ctx context.Context, sub, resource, verb, ns string) (bool, error) {
	if resource == "volumes" {
		return false, nil
	}
	return castScoped{}.Authorize(ctx, sub, resource, verb, ns)
}

// The --detach path must run verb admission synchronously too: the
// background apply has no subject, so a check there would never fire.
func TestCast_DetachRunsVerbAdmission(t *testing.T) {
	c, _ := newTestController(t)
	c.SetAdmission(authz.NewGate(noVolumes{}))
	vol := types.OwnerRef{ResourceType: types.ResourceTypeVolume, Namespace: "default", Name: "data"}
	_, _, err := c.Cast(authctx.WithSubject(context.Background(), "ci"), release.ReleaseSpec{
		Name: "app", Namespace: "default", Detach: true,
		Resources: []release.DesiredResource{{Ref: vol}},
	}, Payloads{Volumes: map[string]*types.Volume{vol.Key(): {Name: "data", Namespace: "default", StorageClassName: "local", Size: "1Gi"}}})
	if !authz.IsDenied(err) {
		t.Fatalf("expected a synchronous volumes denial on the detach path, got %v", err)
	}
	if _, err := c.releases.GetByName(context.Background(), "default", "app"); err == nil {
		t.Fatal("a denied detached cast recorded a release")
	}
}

// oneVerb grants a single verb on services in "default" and nothing else,
// so a test can tell create from update.
type oneVerb string

func (v oneVerb) Authorize(_ context.Context, _, resource, verb, ns string) (bool, error) {
	return ns == "default" && resource == "services" && verb == string(v), nil
}

func TestCast_CreateAndUpdateAreDistinctVerbs(t *testing.T) {
	web := svcRef("default", "web")
	spec := release.ReleaseSpec{Name: "app", Namespace: "default", Resources: []release.DesiredResource{{Ref: web}}}
	payloads := func() Payloads {
		return Payloads{Services: map[string]*types.Service{web.Key(): {Name: "web", Namespace: "default"}}}
	}
	ci := authctx.WithSubject(context.Background(), "ci")

	c, _ := newTestController(t)
	c.SetAdmission(authz.NewGate(oneVerb("update")))
	if _, _, err := c.Cast(ci, spec, payloads()); !authz.IsDenied(err) {
		t.Fatalf("update-only token created a service: %v", err)
	}

	c, _ = newTestController(t)
	if _, _, err := c.Cast(context.Background(), spec, payloads()); err != nil {
		t.Fatalf("seed cast: %v", err)
	}
	c.SetAdmission(authz.NewGate(oneVerb("create")))
	if _, _, err := c.Cast(ci, spec, payloads()); !authz.IsDenied(err) {
		t.Fatalf("create-only token updated a service: %v", err)
	}
}

// A dry-run plan reveals which resources exist and which release owns
// them, so it needs read access in every namespace it names.
func TestPlan_NeedsReadInEachNamespace(t *testing.T) {
	c, _ := newTestController(t)
	c.SetAdmission(authz.NewGate(castScoped{}))
	ci := authctx.WithSubject(context.Background(), "ci")
	other := svcRef("prod", "web")
	_, err := c.Plan(ci, release.ReleaseSpec{
		Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: other}},
	})
	if !authz.IsDenied(err) {
		t.Fatalf("expected a read denial in prod, got %v", err)
	}
	if _, err := c.Plan(ci, release.ReleaseSpec{
		Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: svcRef("default", "web")}},
	}); err != nil {
		t.Fatalf("plan in an allowed namespace denied: %v", err)
	}
}

// A cast that references another namespace's secret needs secrets:reveal
// there, checked before anything is applied.
func TestCast_CrossNamespaceSecretRefNeedsReveal(t *testing.T) {
	c, _ := newTestController(t)
	c.SetAdmission(authz.NewGate(castScoped{}))
	web := svcRef("default", "web")
	_, _, err := c.Cast(authctx.WithSubject(context.Background(), "ci"), release.ReleaseSpec{
		Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: web}},
	}, Payloads{Services: map[string]*types.Service{web.Key(): {
		Name: "web", Namespace: "default",
		Env: map[string]string{"DB": "{{secret:db.prod.rune/password}}"},
	}}})
	if !authz.IsDenied(err) {
		t.Fatalf("expected a secrets:reveal denial in prod, got %v", err)
	}
	if _, err := c.releases.GetByName(context.Background(), "default", "app"); err == nil {
		t.Fatal("a denied cast recorded a release")
	}
}
