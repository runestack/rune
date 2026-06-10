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
	events        []string
	pruned        []types.OwnerRef
	reverted      []types.OwnerRef
	verified      bool
	failApplyKey  string
	failPruneKey  string
	failRevertKey string
	failVerify    bool
}

func (a *fakeApplier) Apply(_ context.Context, c PlannedChange) error {
	if a.failApplyKey != "" && c.Ref.Key() == a.failApplyKey {
		return errors.New("apply boom")
	}
	a.events = append(a.events, "apply:"+c.Ref.Key())
	return nil
}

func (a *fakeApplier) Prune(_ context.Context, ref types.OwnerRef) error {
	if a.failPruneKey != "" && ref.Key() == a.failPruneKey {
		return errors.New("prune boom")
	}
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

func (a *fakeApplier) Revert(_ context.Context, ref types.OwnerRef) error {
	if a.failRevertKey != "" && ref.Key() == a.failRevertKey {
		return errors.New("revert boom")
	}
	a.events = append(a.events, "revert:"+ref.Key())
	a.reverted = append(a.reverted, ref)
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

// --- atomic rollback (Decision D3, --atomic) ---

// An atomic apply failure reverts everything executed so far — including the
// failing change itself (it may have half-applied) — in reverse order.
func TestReconcile_Atomic_ApplyFailureReverts(t *testing.T) {
	creds, web := secretRef("default", "creds"), svcRef("default", "web")
	applier := &fakeApplier{failApplyKey: web.Key()} // secret lands, service fails
	recs := newFakeRecords()

	rel, _, err := Reconcile(context.Background(), ReleaseSpec{
		Name: "rel", Namespace: "default", Atomic: true,
		Resources: desired(web, creds),
	}, recs, fakeLive{}, applier)
	if err == nil {
		t.Fatal("expected apply error")
	}
	if rel.Status != types.ReleaseStatusFailed {
		t.Errorf("status: want failed, got %q", rel.Status)
	}
	// Reverse execution order: the failing service first, then the secret.
	want := []string{"apply:secret/default/creds", "revert:service/default/web", "revert:secret/default/creds"}
	if strings.Join(applier.events, ",") != strings.Join(want, ",") {
		t.Errorf("event order:\n got %v\nwant %v", applier.events, want)
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("error should note the rollback, got: %v", err)
	}
	// First install fully rolled back: the failed record owns nothing.
	if len(rel.Owns) != 0 {
		t.Errorf("owns after first-install rollback: want 0, got %v", rel.Owns)
	}
}

// An atomic verify failure reverts every applied change.
func TestReconcile_Atomic_VerifyFailureReverts(t *testing.T) {
	creds, web := secretRef("default", "creds"), svcRef("default", "web")
	applier := &fakeApplier{failVerify: true}

	rel, _, err := Reconcile(context.Background(), ReleaseSpec{
		Name: "rel", Namespace: "default", Atomic: true,
		Resources: desired(web, creds),
	}, newFakeRecords(), fakeLive{}, applier)
	if err == nil {
		t.Fatal("expected verify error")
	}
	if rel.Status != types.ReleaseStatusFailed {
		t.Errorf("status: want failed, got %q", rel.Status)
	}
	want := []string{
		"apply:secret/default/creds", "apply:service/default/web",
		"revert:service/default/web", "revert:secret/default/creds",
	}
	if strings.Join(applier.events, ",") != strings.Join(want, ",") {
		t.Errorf("event order:\n got %v\nwant %v", applier.events, want)
	}
}

// An atomic prune failure reverts the whole revision — applies and any prunes
// that already ran — and restores the previous revision's owned set on the
// failed record (live state is back to the previous revision's truth).
func TestReconcile_Atomic_PruneFailureRevertsAll(t *testing.T) {
	a, b := svcRef("default", "a"), svcRef("default", "b")
	recs := newFakeRecords()
	prevOwns := []types.OwnerRef{a, b}
	recs.rels["default/rel"] = &types.Release{
		Name: "rel", Namespace: "default", Revision: 1,
		Status: types.ReleaseStatusDeployed, Owns: prevOwns,
	}
	applier := &fakeApplier{failPruneKey: b.Key()} // b dropped from desired → prune fails

	rel, _, err := Reconcile(context.Background(), ReleaseSpec{
		Name: "rel", Namespace: "default", Atomic: true, Resources: desired(a),
	}, recs, fakeLive{}, applier)
	if err == nil {
		t.Fatal("expected prune error")
	}
	if rel.Status != types.ReleaseStatusFailed {
		t.Errorf("status: want failed, got %q", rel.Status)
	}
	want := []string{
		"apply:service/default/a", "verify",
		"revert:service/default/b", "revert:service/default/a",
	}
	if strings.Join(applier.events, ",") != strings.Join(want, ",") {
		t.Errorf("event order:\n got %v\nwant %v", applier.events, want)
	}
	// The failed record's owned set reflects restored reality: the previous
	// revision's resources (including b, which the failed prune would have
	// removed from Owns).
	if len(rel.Owns) != len(prevOwns) {
		t.Errorf("owns after rollback: want %d (previous revision's), got %v", len(prevOwns), rel.Owns)
	}
}

// A failing revert doesn't strand the rest of the rollback: remaining changes
// still revert, and the error carries both the cause and the revert failure.
func TestReconcile_Atomic_RevertFailureAggregates(t *testing.T) {
	creds, web := secretRef("default", "creds"), svcRef("default", "web")
	applier := &fakeApplier{failVerify: true, failRevertKey: web.Key()}

	rel, _, err := Reconcile(context.Background(), ReleaseSpec{
		Name: "rel", Namespace: "default", Atomic: true,
		Resources: desired(web, creds),
	}, newFakeRecords(), fakeLive{}, applier)
	if err == nil {
		t.Fatal("expected error")
	}
	if rel.Status != types.ReleaseStatusFailed {
		t.Errorf("status: want failed, got %q", rel.Status)
	}
	// The secret still reverted despite the service revert failing.
	if len(applier.reverted) != 1 || applier.reverted[0].Key() != creds.Key() {
		t.Errorf("secret should still revert, got %v", applier.reverted)
	}
	for _, fragment := range []string{"unhealthy", "rollback incomplete", web.Key()} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error should contain %q, got: %v", fragment, err)
		}
	}
}

// Without Atomic, a failure reverts nothing (today's leave-as-failed default).
func TestReconcile_NonAtomic_DoesNotRevert(t *testing.T) {
	web := svcRef("default", "web")
	applier := &fakeApplier{failVerify: true}

	rel, _, err := Reconcile(context.Background(), ReleaseSpec{
		Name: "rel", Namespace: "default", Resources: desired(web),
	}, newFakeRecords(), fakeLive{}, applier)
	if err == nil {
		t.Fatal("expected verify error")
	}
	if rel.Status != types.ReleaseStatusFailed {
		t.Errorf("status: want failed, got %q", rel.Status)
	}
	if len(applier.reverted) != 0 {
		t.Errorf("non-atomic failure must not revert, got %v", applier.reverted)
	}
}
