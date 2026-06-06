// Package release implements the stateful runeset release model: the 3-way
// reconcile plan that turns a rendered cast into create/update/prune/adopt
// actions against the cluster.
//
// Design: _docs/plugins/RUNESET_STATEFUL_RELEASES.md and CAST_REFACTOR_PLAN.md.
// This package is the pure planning core — it computes WHAT a cast will do; the
// server-side ReleaseController executes the plan (apply, verify, prune-last).
package release

import "github.com/runestack/rune/pkg/types"

// Action is the reconcile verb planned for a single resource.
type Action string

const (
	// ActionCreate — resource is desired and does not exist; create and own it.
	ActionCreate Action = "create"
	// ActionUpdate — resource is desired and already owned by this release.
	ActionUpdate Action = "update"
	// ActionPrune — resource was owned by the previous revision but is no longer
	// desired; delete it (runs last, after verify — Decision D3).
	ActionPrune Action = "prune"
	// ActionAdopt — resource exists but is unmanaged or owned elsewhere, and
	// --adopt was given; take ownership.
	ActionAdopt Action = "adopt"
	// ActionReference — inherently-shared cluster-scoped kind (StorageClass,
	// Namespace): ensure it exists but never own or prune it (Decision D2).
	ActionReference Action = "reference"
)

// DesiredResource is one resource the rendered release wants to exist.
type DesiredResource struct {
	Ref types.OwnerRef
	// ContentHash optionally lets the planner skip no-op updates. Empty means
	// "always treat as a potential update" (a finer no-op check is a TODO).
	ContentHash string
}

// Conflict marks a planned change that cannot be applied as-is: the live
// resource is owned by a different release (or unmanaged) and adoption was not
// requested. A plan containing any Conflict is not applyable.
type Conflict struct {
	OwnedBy *types.OwnedBy
	Reason  string
}

// PlannedChange pairs a resource ref with its planned action.
type PlannedChange struct {
	Ref      types.OwnerRef
	Action   Action
	Conflict *Conflict
}

// Plan is the full set of changes a cast revision will make, in apply order
// (desired resources in input order, then prunes).
type Plan struct {
	Release   string
	Namespace string
	Changes   []PlannedChange
}

// Applyable reports whether the plan is free of unresolved conflicts.
func (p *Plan) Applyable() bool {
	for i := range p.Changes {
		if p.Changes[i].Conflict != nil {
			return false
		}
	}
	return true
}

// Counts tallies changes by action (for the cast plan display block).
func (p *Plan) Counts() map[Action]int {
	out := make(map[Action]int)
	for i := range p.Changes {
		out[p.Changes[i].Action]++
	}
	return out
}

// HasPrune reports whether the plan deletes anything. Used to refuse --detach on
// destructive plans (Decision C3: create/update-only detach).
func (p *Plan) HasPrune() bool {
	for i := range p.Changes {
		if p.Changes[i].Action == ActionPrune {
			return true
		}
	}
	return false
}

// LiveState is the observed live state of a single resource.
type LiveState struct {
	Exists bool
	// OwnedBy is the OwnedBy stamp found on the live resource, or nil if the
	// resource is unmanaged (exists but carries no release ownership).
	OwnedBy *types.OwnedBy
}

// LiveLookup reports the live state of a resource. Implemented server-side
// against the store/orchestrator; faked in tests.
type LiveLookup interface {
	Lookup(ref types.OwnerRef) (LiveState, error)
}

// Options tune planning.
type Options struct {
	// Adopt allows taking over resources that exist but are unmanaged or owned
	// by a different release (the --adopt flag).
	Adopt bool
}

// isSharedClusterKind returns true for inherently-shared, cluster-scoped kinds
// that are referenced rather than owned (Decision D2).
func isSharedClusterKind(rt types.ResourceType) bool {
	switch rt {
	case types.ResourceTypeStorageClass, types.ResourceTypeNamespace:
		return true
	default:
		return false
	}
}

// BuildPlan computes the 3-way reconcile between the desired set, the current
// release record (nil for a first install), and live state.
//
// Rules:
//   - Shared cluster-scoped kinds (StorageClass, Namespace) are ActionReference:
//     ensured-present, never owned or pruned.
//   - A desired resource already in current.Owns → ActionUpdate.
//   - A desired resource that doesn't exist live → ActionCreate.
//   - A desired resource that exists live but isn't ours → ActionAdopt if opts.Adopt,
//     else a Conflict.
//   - A resource in current.Owns no longer desired → ActionPrune.
func BuildPlan(releaseName, namespace string, desired []DesiredResource, current *types.Release, live LiveLookup, opts Options) (*Plan, error) {
	plan := &Plan{Release: releaseName, Namespace: namespace}

	// Index the previous revision's owned set.
	owned := map[string]bool{}
	if current != nil {
		for _, ref := range current.Owns {
			owned[ref.Key()] = true
		}
	}

	desiredKeys := map[string]bool{}

	for _, d := range desired {
		desiredKeys[d.Ref.Key()] = true

		// Shared cluster-scoped kinds are referenced, never owned.
		if isSharedClusterKind(d.Ref.ResourceType) {
			plan.Changes = append(plan.Changes, PlannedChange{Ref: d.Ref, Action: ActionReference})
			continue
		}

		// Already owned by this release → update.
		if owned[d.Ref.Key()] {
			plan.Changes = append(plan.Changes, PlannedChange{Ref: d.Ref, Action: ActionUpdate})
			continue
		}

		state, err := live.Lookup(d.Ref)
		if err != nil {
			return nil, err
		}

		if !state.Exists {
			plan.Changes = append(plan.Changes, PlannedChange{Ref: d.Ref, Action: ActionCreate})
			continue
		}

		// Exists live but not in our record.
		switch {
		case state.OwnedBy != nil && state.OwnedBy.Release == releaseName:
			// We already own it (record/live drift) — reconcile as update.
			// TODO: surface this as repaired drift in the plan display.
			plan.Changes = append(plan.Changes, PlannedChange{Ref: d.Ref, Action: ActionUpdate})
		case opts.Adopt:
			plan.Changes = append(plan.Changes, PlannedChange{Ref: d.Ref, Action: ActionAdopt})
		default:
			reason := "resource exists and is unmanaged; pass --adopt to take ownership"
			if state.OwnedBy != nil {
				reason = "resource is owned by release \"" + state.OwnedBy.Release + "\"; pass --adopt to take ownership"
			}
			plan.Changes = append(plan.Changes, PlannedChange{
				Ref:      d.Ref,
				Action:   ActionAdopt,
				Conflict: &Conflict{OwnedBy: state.OwnedBy, Reason: reason},
			})
		}
	}

	// Prune: previously owned, no longer desired. Shared kinds are never owned,
	// so they never reach here.
	if current != nil {
		for _, ref := range current.Owns {
			if desiredKeys[ref.Key()] {
				continue
			}
			plan.Changes = append(plan.Changes, PlannedChange{Ref: ref, Action: ActionPrune})
		}
	}

	return plan, nil
}
