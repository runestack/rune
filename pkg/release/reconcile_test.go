package release

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/types"
)

func secretRef(ns, name string) types.OwnerRef {
	return types.OwnerRef{ResourceType: types.ResourceTypeSecret, Namespace: ns, Name: name}
}

// fakeRecords is an in-memory ReleaseRecords.
type fakeRecords struct{ rels map[string]*types.Release }

func newFakeRecords() *fakeRecords { return &fakeRecords{rels: map[string]*types.Release{}} }

func (f *fakeRecords) Get(_ context.Context, ns, name string) (*types.Release, bool, error) {
	r, ok := f.rels[ns+"/"+name]
	if !ok {
		return nil, false, nil
	}
	return r, true, nil
}

func (f *fakeRecords) Save(_ context.Context, rel *types.Release) error {
	cp := *rel
	f.rels[rel.Namespace+"/"+rel.Name] = &cp
	return nil
}

// fakeApplier records an ordered event log so tests can assert prune-last.
type fakeApplier struct {
	events       []string
	pruned       []types.OwnerRef
	verified     bool
	failApplyKey string
	failVerify   bool
}

func (a *fakeApplier) Apply(_ context.Context, c PlannedChange) error {
	if a.failApplyKey != "" && c.Ref.Key() == a.failApplyKey {
		return errors.New("apply boom")
	}
	a.events = append(a.events, "apply:"+c.Ref.Key())
	return nil
}

func (a *fakeApplier) Prune(_ context.Context, ref types.OwnerRef) error {
	a.events = append(a.events, "prune:"+ref.Key())
	a.pruned = append(a.pruned, ref)
	return nil
}

func (a *fakeApplier) Verify(_ context.Context, _ []types.OwnerRef) error {
	if a.failVerify {
		return errors.New("unhealthy")
	}
	a.events = append(a.events, "verify")
	a.verified = true
	return nil
}

func TestReconcile_FirstInstall_OrdersAndDeploys(t *testing.T) {
	spec := ReleaseSpec{
		Name:      "rel",
		Namespace: "default",
		// Intentionally out of dependency order to prove the reconciler sorts.
		Resources: desired(svcRef("default", "web"), secretRef("default", "creds")),
	}
	recs := newFakeRecords()
	applier := &fakeApplier{}

	rel, _, err := Reconcile(context.Background(), spec, recs, fakeLive{}, applier)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rel.Status != types.ReleaseStatusDeployed {
		t.Errorf("status: want deployed, got %q", rel.Status)
	}
	if rel.Revision != 1 {
		t.Errorf("revision: want 1, got %d", rel.Revision)
	}
	// Secret applies before Service regardless of input order.
	want := []string{"apply:secret/default/creds", "apply:service/default/web", "verify"}
	if strings.Join(applier.events, ",") != strings.Join(want, ",") {
		t.Errorf("event order:\n got %v\nwant %v", applier.events, want)
	}
	if len(rel.Owns) != 2 {
		t.Errorf("owns: want 2, got %d", len(rel.Owns))
	}
}

func TestReconcile_Upgrade_PrunesAfterVerify(t *testing.T) {
	a, b := svcRef("default", "a"), svcRef("default", "b")
	recs := newFakeRecords()
	recs.rels["default/rel"] = &types.Release{
		Name: "rel", Namespace: "default", Revision: 1,
		Status: types.ReleaseStatusDeployed, Owns: []types.OwnerRef{a, b},
	}
	applier := &fakeApplier{}

	rel, _, err := Reconcile(context.Background(), ReleaseSpec{
		Name: "rel", Namespace: "default", Resources: desired(a), // b dropped
	}, recs, fakeLive{}, applier)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rel.Revision != 2 {
		t.Errorf("revision: want 2, got %d", rel.Revision)
	}
	// Verify must precede the prune of b.
	want := []string{"apply:service/default/a", "verify", "prune:service/default/b"}
	if strings.Join(applier.events, ",") != strings.Join(want, ",") {
		t.Errorf("event order:\n got %v\nwant %v", applier.events, want)
	}
}

func TestReconcile_ApplyFailure_DoesNotPrune(t *testing.T) {
	a, b := svcRef("default", "a"), svcRef("default", "b")
	recs := newFakeRecords()
	recs.rels["default/rel"] = &types.Release{
		Name: "rel", Namespace: "default", Revision: 1,
		Status: types.ReleaseStatusDeployed, Owns: []types.OwnerRef{a, b},
	}
	applier := &fakeApplier{failApplyKey: a.Key()}

	rel, _, err := Reconcile(context.Background(), ReleaseSpec{
		Name: "rel", Namespace: "default", Resources: desired(a),
	}, recs, fakeLive{}, applier)
	if err == nil {
		t.Fatal("expected apply error")
	}
	if rel.Status != types.ReleaseStatusFailed {
		t.Errorf("status: want failed, got %q", rel.Status)
	}
	if len(applier.pruned) != 0 {
		t.Errorf("nothing should be pruned on apply failure, got %v", applier.pruned)
	}
}

func TestReconcile_DetachRefusesPrune(t *testing.T) {
	a, b := svcRef("default", "a"), svcRef("default", "b")
	recs := newFakeRecords()
	recs.rels["default/rel"] = &types.Release{
		Name: "rel", Namespace: "default", Revision: 1, Owns: []types.OwnerRef{a, b},
	}
	_, _, err := Reconcile(context.Background(), ReleaseSpec{
		Name: "rel", Namespace: "default", Resources: desired(a), Detach: true,
	}, recs, fakeLive{}, &fakeApplier{})
	if !errors.Is(err, ErrDetachWouldPrune) {
		t.Errorf("want ErrDetachWouldPrune, got %v", err)
	}
}

func TestReconcile_ConflictIsNotApplied(t *testing.T) {
	a := svcRef("default", "a")
	live := fakeLive{a.Key(): {Exists: true, OwnedBy: nil}} // unmanaged collision
	_, _, err := Reconcile(context.Background(), ReleaseSpec{
		Name: "rel", Namespace: "default", Resources: desired(a),
	}, newFakeRecords(), live, &fakeApplier{})
	if !errors.Is(err, ErrConflicts) {
		t.Errorf("want ErrConflicts, got %v", err)
	}
}

// Prepare records the pending intent before any apply; Execute then deploys.
// This split is what lets --detach return the pending release immediately and
// run Execute in the background (C3-a).
func TestPrepare_RecordsPendingThenExecuteDeploys(t *testing.T) {
	a := svcRef("default", "web")
	recs := newFakeRecords()
	prep, err := Prepare(context.Background(), ReleaseSpec{
		Name: "rel", Namespace: "default", Resources: desired(a),
	}, recs, fakeLive{})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prep.Release == nil || prep.Release.Status != types.ReleaseStatusPending {
		t.Fatalf("want pending release, got %+v", prep.Release)
	}
	// Pending intent must be persisted before any apply (so a detached caller's
	// `release get` sees it immediately).
	if got := recs.rels["default/rel"]; got == nil || got.Status != types.ReleaseStatusPending {
		t.Errorf("pending record not persisted: %+v", got)
	}
	applier := &fakeApplier{}
	rel, err := prep.Execute(context.Background(), applier)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rel.Status != types.ReleaseStatusDeployed {
		t.Errorf("want deployed after execute, got %q", rel.Status)
	}
	if !applier.verified {
		t.Errorf("execute should have verified owned resources")
	}
}

// A detach plan that would prune is rejected at Prepare time, before any
// background handoff (C3).
func TestPrepare_DetachWithPruneRefused(t *testing.T) {
	a, b := svcRef("default", "a"), svcRef("default", "b")
	recs := newFakeRecords()
	recs.rels["default/rel"] = &types.Release{
		Name: "rel", Namespace: "default", Revision: 1, Owns: []types.OwnerRef{a, b},
	}
	_, err := Prepare(context.Background(), ReleaseSpec{
		Name: "rel", Namespace: "default", Resources: desired(a), Detach: true,
	}, recs, fakeLive{})
	if !errors.Is(err, ErrDetachWouldPrune) {
		t.Errorf("want ErrDetachWouldPrune, got %v", err)
	}
}
