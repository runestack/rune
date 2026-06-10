package releasectl

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/crypto"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

func newTestController(t *testing.T) (*Controller, *store.TestStore) {
	t.Helper()
	kek, err := crypto.RandomBytes(32)
	if err != nil {
		t.Fatalf("generate KEK: %v", err)
	}
	st := store.NewTestStoreWithOptions(store.StoreOptions{
		KEKBytes:                kek,
		SecretEncryptionEnabled: true,
		SecretLimits: store.Limits{
			MaxObjectBytes:   1 << 20,
			MaxKeyNameLength: 256,
		},
	})
	orch := orchestrator.NewFakeOrchestrator()
	c := NewController(orch, st, log.NewTestLogger())
	return c, st
}

func svcRef(ns, name string) types.OwnerRef {
	return types.OwnerRef{ResourceType: types.ResourceTypeService, Namespace: ns, Name: name}
}

func secretRef(ns, name string) types.OwnerRef {
	return types.OwnerRef{ResourceType: types.ResourceTypeSecret, Namespace: ns, Name: name}
}

// TestCast_CreatesDeployedRelease asserts a first cast produces a deployed
// Release record whose Owns set matches the desired resources.
func TestCast_CreatesDeployedRelease(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	sref := svcRef("default", "web")
	scrref := secretRef("default", "creds")

	spec := release.ReleaseSpec{
		Name:      "app",
		Namespace: "default",
		Resources: []release.DesiredResource{{Ref: sref}, {Ref: scrref}},
	}
	payloads := Payloads{
		Services: map[string]*types.Service{
			sref.Key(): {Name: "web", Namespace: "default"},
		},
		Secrets: map[string]*types.Secret{
			scrref.Key(): {Name: "creds", Namespace: "default", Type: "static"},
		},
	}

	rel, plan, err := c.Cast(ctx, spec, payloads)
	if err != nil {
		t.Fatalf("Cast: %v", err)
	}
	if rel.Status != types.ReleaseStatusDeployed {
		t.Errorf("status: want deployed, got %q", rel.Status)
	}
	if rel.Revision != 1 {
		t.Errorf("revision: want 1, got %d", rel.Revision)
	}
	if len(rel.Owns) != 2 {
		t.Fatalf("owns: want 2, got %d (%v)", len(rel.Owns), rel.Owns)
	}
	if plan == nil || !plan.Applyable() {
		t.Errorf("plan should be applyable")
	}

	// The persisted record should be retrievable and deployed.
	repo := repos.NewReleaseRepo(c.releases.Core())
	got, err := repo.GetByName(ctx, "default", "app")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.Status != types.ReleaseStatusDeployed {
		t.Errorf("persisted status: want deployed, got %q", got.Status)
	}

	// The owned service should carry the OwnedBy stamp.
	svc, err := c.orch.GetService(ctx, "default", "web")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if svc.Metadata == nil || svc.Metadata.OwnedBy == nil {
		t.Fatalf("service missing OwnedBy stamp")
	}
	if svc.Metadata.OwnedBy.Release != "app" || svc.Metadata.OwnedBy.Revision != 1 {
		t.Errorf("OwnedBy: want app/1, got %s/%d", svc.Metadata.OwnedBy.Release, svc.Metadata.OwnedBy.Revision)
	}
}

// TestUninstall_SoftMarks asserts uninstall prunes owned resources and leaves a
// soft-marked tombstone by default (Decision D4).
func TestUninstall_SoftMarks(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	sref := svcRef("default", "web")
	scrref := secretRef("default", "creds")
	spec := release.ReleaseSpec{
		Name:      "app",
		Namespace: "default",
		Resources: []release.DesiredResource{{Ref: sref}, {Ref: scrref}},
	}
	payloads := Payloads{
		Services: map[string]*types.Service{sref.Key(): {Name: "web", Namespace: "default"}},
		Secrets:  map[string]*types.Secret{scrref.Key(): {Name: "creds", Namespace: "default", Type: "static"}},
	}
	if _, _, err := c.Cast(ctx, spec, payloads); err != nil {
		t.Fatalf("Cast: %v", err)
	}

	if err := c.Uninstall(ctx, "default", "app", UninstallOptions{}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	repo := repos.NewReleaseRepo(c.releases.Core())
	got, err := repo.GetByName(ctx, "default", "app")
	if err != nil {
		t.Fatalf("record should remain as tombstone: %v", err)
	}
	if got.Status != types.ReleaseStatusUninstalled {
		t.Errorf("status: want uninstalled, got %q", got.Status)
	}
	if len(got.Owns) != 0 {
		t.Errorf("owns should be cleared, got %v", got.Owns)
	}

	// Owned service should be gone.
	if _, err := c.orch.GetService(ctx, "default", "web"); err == nil {
		t.Errorf("service should have been pruned")
	}
	// Owned secret should be gone.
	if _, err := c.secrets.Get(ctx, "default", "creds"); err == nil {
		t.Errorf("secret should have been pruned")
	}
}

// TestUninstall_Purge removes the record entirely.
func TestUninstall_Purge(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	sref := svcRef("default", "web")
	spec := release.ReleaseSpec{
		Name:      "app",
		Namespace: "default",
		Resources: []release.DesiredResource{{Ref: sref}},
	}
	payloads := Payloads{Services: map[string]*types.Service{sref.Key(): {Name: "web", Namespace: "default"}}}
	if _, _, err := c.Cast(ctx, spec, payloads); err != nil {
		t.Fatalf("Cast: %v", err)
	}

	if err := c.Uninstall(ctx, "default", "app", UninstallOptions{Purge: true}); err != nil {
		t.Fatalf("Uninstall purge: %v", err)
	}
	if _, err := c.releases.GetByName(ctx, "default", "app"); err == nil {
		t.Errorf("record should have been purged")
	}
}

// TestPlan_NoApply computes a plan without persisting or applying anything.
func TestPlan_NoApply(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	sref := svcRef("default", "web")
	plan, err := c.Plan(ctx, release.ReleaseSpec{
		Name:      "app",
		Namespace: "default",
		Resources: []release.DesiredResource{{Ref: sref}},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0].Action != release.ActionCreate {
		t.Errorf("want a single create change, got %v", plan.Changes)
	}
	// Plan must not have persisted a record.
	if _, err := c.releases.GetByName(ctx, "default", "app"); err == nil {
		t.Errorf("Plan should not persist a release record")
	}
}

// --- atomic rollback against the real applier (--atomic, D3) ---

// An atomic cast whose apply fails midway deletes the resources it created.
// The service payload is deliberately missing, so the secret (rank 4) applies
// first and the service (rank 5) fails — the secret must then be rolled back.
func TestCast_Atomic_ApplyFailureCleansUpCreates(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	sref := svcRef("default", "web")
	scrref := secretRef("default", "creds")
	spec := release.ReleaseSpec{
		Name:      "app",
		Namespace: "default",
		Atomic:    true,
		Resources: []release.DesiredResource{{Ref: sref}, {Ref: scrref}},
	}
	payloads := Payloads{
		// No service payload → Apply fails after the secret has been created.
		Secrets: map[string]*types.Secret{
			scrref.Key(): {Name: "creds", Namespace: "default", Type: "static",
				Data: map[string]string{"password": "hunter2"}},
		},
	}

	rel, _, err := c.Cast(ctx, spec, payloads)
	if err == nil {
		t.Fatal("expected cast to fail (missing service payload)")
	}
	if rel == nil || rel.Status != types.ReleaseStatusFailed {
		t.Fatalf("want failed release record, got %+v", rel)
	}
	// The created secret must be gone again.
	if _, err := c.secrets.Get(ctx, "default", "creds"); err == nil {
		t.Errorf("secret should have been rolled back (deleted)")
	}
	// Rolled back to nothing: the failed record owns nothing.
	if len(rel.Owns) != 0 {
		t.Errorf("owns after first-install rollback: want 0, got %v", rel.Owns)
	}
}

// An atomic upgrade whose apply fails after updating a secret restores the
// secret's previous revision — INCLUDING its Data values (json:"-", stored
// encrypted separately) and the previous OwnedBy stamp.
func TestCast_Atomic_UpdateFailureRestoresSecret(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	scrref := secretRef("default", "creds")
	v1 := Payloads{
		Secrets: map[string]*types.Secret{
			scrref.Key(): {Name: "creds", Namespace: "default", Type: "static",
				Data: map[string]string{"password": "v1-value"}},
		},
	}
	if _, _, err := c.Cast(ctx, release.ReleaseSpec{
		Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: scrref}},
	}, v1); err != nil {
		t.Fatalf("cast v1: %v", err)
	}

	// Revision 2 (atomic): update the secret AND add a service whose payload is
	// missing — the secret update lands, then the service apply fails.
	sref := svcRef("default", "web")
	v2 := Payloads{
		Secrets: map[string]*types.Secret{
			scrref.Key(): {Name: "creds", Namespace: "default", Type: "static",
				Data: map[string]string{"password": "v2-value"}},
		},
	}
	rel, _, err := c.Cast(ctx, release.ReleaseSpec{
		Name: "app", Namespace: "default", Atomic: true,
		Resources: []release.DesiredResource{{Ref: scrref}, {Ref: sref}},
	}, v2)
	if err == nil {
		t.Fatal("expected cast v2 to fail (missing service payload)")
	}
	if rel == nil || rel.Status != types.ReleaseStatusFailed {
		t.Fatalf("want failed release record, got %+v", rel)
	}

	// The secret must be back at revision 1's state: v1 Data and OwnedBy rev 1.
	got, err := c.secrets.Get(ctx, "default", "creds")
	if err != nil {
		t.Fatalf("secret should still exist after rollback: %v", err)
	}
	if got.Data["password"] != "v1-value" {
		t.Errorf("secret data: want restored %q, got %q", "v1-value", got.Data["password"])
	}
	if got.OwnedBy == nil || got.OwnedBy.Revision != 1 {
		t.Errorf("OwnedBy: want revision 1 restored, got %+v", got.OwnedBy)
	}
	// The failed record's owned set reflects restored reality (revision 1's).
	if len(rel.Owns) != 1 || rel.Owns[0].Key() != scrref.Key() {
		t.Errorf("owns after rollback: want just the secret, got %v", rel.Owns)
	}
}

// Without --atomic the same failure leaves the half-applied state in place
// (today's default), so operators can inspect what landed.
func TestCast_NonAtomic_LeavesPartialState(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	sref := svcRef("default", "web")
	scrref := secretRef("default", "creds")
	payloads := Payloads{
		Secrets: map[string]*types.Secret{
			scrref.Key(): {Name: "creds", Namespace: "default", Type: "static",
				Data: map[string]string{"password": "hunter2"}},
		},
	}
	rel, _, err := c.Cast(ctx, release.ReleaseSpec{
		Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: sref}, {Ref: scrref}},
	}, payloads)
	if err == nil {
		t.Fatal("expected cast to fail (missing service payload)")
	}
	if rel == nil || rel.Status != types.ReleaseStatusFailed {
		t.Fatalf("want failed release record, got %+v", rel)
	}
	// The created secret stays (no rollback by default).
	if _, err := c.secrets.Get(ctx, "default", "creds"); err != nil {
		t.Errorf("secret should remain without --atomic: %v", err)
	}
}
