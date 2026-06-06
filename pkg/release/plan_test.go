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
	// b is dropped from desired → should prune.
	p, err := BuildPlan("rel", "default", desired(a), current, fakeLive{}, Options{})
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
