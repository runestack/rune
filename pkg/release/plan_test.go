package release

import (
	"testing"

	"github.com/runestack/rune/pkg/types"
)

// fakeLive is an in-memory LiveLookup keyed by OwnerRef.Key().
type fakeLive map[string]LiveState

func (f fakeLive) Lookup(ref types.OwnerRef) (LiveState, error) {
	return f[ref.Key()], nil
}

func svcRef(ns, name string) types.OwnerRef {
	return types.OwnerRef{ResourceType: types.ResourceTypeService, Namespace: ns, Name: name}
}

func desired(refs ...types.OwnerRef) []DesiredResource {
	out := make([]DesiredResource, 0, len(refs))
	for _, r := range refs {
		out = append(out, DesiredResource{Ref: r})
	}
	return out
}

// actionFor returns the planned action for a ref, or "" if absent.
func actionFor(p *Plan, ref types.OwnerRef) Action {
	for i := range p.Changes {
		if p.Changes[i].Ref.Key() == ref.Key() {
			return p.Changes[i].Action
		}
	}
	return ""
}

func TestBuildPlan_FirstInstall_AllCreate(t *testing.T) {
	a, b := svcRef("default", "a"), svcRef("default", "b")
	p, err := BuildPlan("rel", "default", desired(a, b), nil, fakeLive{}, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := actionFor(p, a); got != ActionCreate {
		t.Errorf("a: want create, got %q", got)
	}
	if got := actionFor(p, b); got != ActionCreate {
		t.Errorf("b: want create, got %q", got)
	}
	if !p.Applyable() {
		t.Errorf("first install should be applyable")
	}
}

func TestBuildPlan_Upgrade_PrunesRemoved(t *testing.T) {
	a, b := svcRef("default", "a"), svcRef("default", "b")
	current := &types.Release{Name: "rel", Namespace: "default", Owns: []types.OwnerRef{a, b}}
	ours := &types.OwnedBy{Release: "rel", Namespace: "default"}
	live := fakeLive{a.Key(): {Exists: true, OwnedBy: ours}, b.Key(): {Exists: true, OwnedBy: ours}}
	// b is dropped from desired → should prune.
	p, err := BuildPlan("rel", "default", desired(a), current, live, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := actionFor(p, a); got != ActionUpdate {
		t.Errorf("a: want update, got %q", got)
	}
	if got := actionFor(p, b); got != ActionPrune {
		t.Errorf("b: want prune, got %q", got)
	}
}

func TestBuildPlan_UnmanagedConflict_RequiresAdopt(t *testing.T) {
	a := svcRef("default", "a")
	live := fakeLive{a.Key(): {Exists: true, OwnedBy: nil}} // exists, unmanaged

	// Without --adopt → conflict, not applyable.
	p, err := BuildPlan("rel", "default", desired(a), nil, live, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Applyable() {
		t.Errorf("unmanaged-collision plan should not be applyable without --adopt")
	}

	// With --adopt → adopt, applyable.
	p2, err := BuildPlan("rel", "default", desired(a), nil, live, Options{Adopt: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := actionFor(p2, a); got != ActionAdopt {
		t.Errorf("a: want adopt, got %q", got)
	}
	if !p2.Applyable() {
		t.Errorf("plan with --adopt should be applyable")
	}
}

func TestBuildPlan_OwnedByOtherRelease_Conflicts(t *testing.T) {
	a := svcRef("default", "a")
	live := fakeLive{a.Key(): {Exists: true, OwnedBy: &types.OwnedBy{Release: "other", Revision: 2, Manager: types.ManagerRuneset}}}
	p, err := BuildPlan("rel", "default", desired(a), nil, live, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Applyable() {
		t.Errorf("resource owned by another release should conflict without --adopt")
	}
}

func TestBuildPlan_SharedKind_IsReferencedNotPruned(t *testing.T) {
	sc := types.OwnerRef{ResourceType: types.ResourceTypeStorageClass, Namespace: "", Name: "fast"}
	// Previously "owned" record shouldn't matter for shared kinds; ensure no prune.
	current := &types.Release{Name: "rel", Namespace: "default", Owns: []types.OwnerRef{}}
	p, err := BuildPlan("rel", "default", desired(sc), current, fakeLive{}, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := actionFor(p, sc); got != ActionReference {
		t.Errorf("storageclass: want reference, got %q", got)
	}
}

// The record says this release owns a, but a was deleted and recreated by
// someone else since: updating it would take it over without --adopt.
func TestBuildPlan_RecordedButReplacedIsAConflict(t *testing.T) {
	a := svcRef("default", "a")
	current := &types.Release{Name: "rel", Namespace: "default", Owns: []types.OwnerRef{a}}
	for name, owner := range map[string]*types.OwnedBy{
		"another release": {Release: "other", Namespace: "default"},
		"unmanaged":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			p, err := BuildPlan("rel", "default", desired(a), current, fakeLive{a.Key(): {Exists: true, OwnedBy: owner}}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if p.Applyable() {
				t.Fatalf("a replaced resource must conflict, got %+v", p.Changes)
			}
		})
	}
}

func TestBuildPlan_RecordedButGoneIsACreate(t *testing.T) {
	a := svcRef("default", "a")
	current := &types.Release{Name: "rel", Namespace: "default", Owns: []types.OwnerRef{a}}
	p, err := BuildPlan("rel", "default", desired(a), current, fakeLive{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := actionFor(p, a); got != ActionCreate {
		t.Errorf("want create, got %q", got)
	}
}

// Two releases may share a name in different namespaces and still write
// into one namespace; the stamp's namespace tells them apart.
func TestBuildPlan_SameNameOtherNamespaceIsNotUs(t *testing.T) {
	a := svcRef("shared", "a")
	live := fakeLive{a.Key(): {Exists: true, OwnedBy: &types.OwnedBy{Release: "app", Namespace: "team-a"}}}
	p, err := BuildPlan("app", "team-b", desired(a), nil, live, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Applyable() {
		t.Fatalf("team-b/app must not update team-a/app's resource, got %+v", p.Changes)
	}

	// A stamp from before the namespace was recorded still matches by name.
	legacy := fakeLive{a.Key(): {Exists: true, OwnedBy: &types.OwnedBy{Release: "app"}}}
	p, err = BuildPlan("app", "team-b", desired(a), nil, legacy, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := actionFor(p, a); got != ActionUpdate {
		t.Errorf("legacy stamp: want update, got %q", got)
	}
}

// A resource the record lists that now belongs to someone else is dropped
// from the release, never deleted.
func TestBuildPlan_DoesNotPruneWhatIsNoLongerOurs(t *testing.T) {
	a, b, c := svcRef("default", "a"), svcRef("default", "b"), svcRef("default", "c")
	current := &types.Release{Name: "rel", Namespace: "default", Owns: []types.OwnerRef{a, b, c}}
	live := fakeLive{
		a.Key(): {Exists: true, OwnedBy: &types.OwnedBy{Release: "other", Namespace: "default"}},
		b.Key(): {Exists: true},
		// c is gone: pruning it is harmless and clears it from the record.
	}
	p, err := BuildPlan("rel", "default", nil, current, live, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := actionFor(p, a); got != "" {
		t.Errorf("a belongs to another release: want no change, got %q", got)
	}
	if got := actionFor(p, b); got != "" {
		t.Errorf("b is unmanaged now: want no change, got %q", got)
	}
	if got := actionFor(p, c); got != ActionPrune {
		t.Errorf("c: want prune, got %q", got)
	}
}
