package controllers

// RUNE-042 Phase 3: the update planner.
//
// A pure function. No store, no context, no clock reads inside — time is an
// input — so the whole of the update's decision-making is table-testable
// without a running orchestrator. This is deliberately the cheapest place to
// retire the design's real risk: the budget arithmetic either preserves
// availability or it does not, and that can be proven here before a single
// line of reconciler wiring exists.
//
// See _docs/designs/RUNE-042-Rolling-Updates.md §7 and §8.1.

import (
	"fmt"
	"sort"
	"time"

	"github.com/runestack/rune/pkg/types"
)

// instanceView is one instance as the planner sees it: its classification
// plus the two readiness facts the budget turns on. Built by newInstanceView
// in the reconciler; constructed directly in tests.
type instanceView struct {
	Instance *types.Instance

	// Class is the broken/outdated/OK verdict (Phase 2).
	Class CompatClass

	// Ready is Status == Running. For a service with a readiness probe that
	// means a probe has passed; without one it means only that the runner
	// accepted the container, which is why MinReady exists.
	Ready bool

	// Available is Ready AND held Running for at least MinReady. This is what
	// "serving" means to the budget.
	Available bool

	// Terminating is an instance already being torn down. It still occupies
	// capacity (so it counts toward the total) but serves nobody and must not
	// be selected for retirement twice. Teardown is synchronous inside a
	// reconcile pass, so in practice the planner rarely observes one; the
	// field exists for correctness, not for a steady state.
	Terminating bool
}

// serving reports whether this instance is actually carrying traffic right
// now — the only notion the availability budget may count.
//
// Being Available is not sufficient: Available derives from the STORE status,
// and a broken instance can be Running there while dead in the runner (that
// is precisely the dead-in-runner case classification reorders for). Counting
// such an instance as serving lets the planner retire a genuinely serving one
// in its place and breach the availability floor — a bug only the floor
// property catches, since the plan still respects the surge cap.
func (v *instanceView) serving() bool {
	return v.Available && !v.Terminating && v.Class != CompatBroken
}

// newInstanceView derives the readiness facts from a live instance.
// A nil LastTransitionAt counts as available: instances predating that field
// must not wedge the first update after an upgrade.
func newInstanceView(inst *types.Instance, class CompatClass, minReady time.Duration, now time.Time) instanceView {
	ready := inst.Status == types.InstanceStatusRunning
	available := ready
	if ready && minReady > 0 && inst.LastTransitionAt != nil {
		available = now.Sub(*inst.LastTransitionAt) >= minReady
	}
	return instanceView{
		Instance:    inst,
		Class:       class,
		Ready:       ready,
		Available:   available,
		Terminating: inst.Status == types.InstanceStatusTerminating,
	}
}

// updateInput is everything the planner needs. Instances is the live set —
// the caller has already filtered Deleted records and Failed tombstones.
type updateInput struct {
	Scale     int
	Params    types.UpdateParams
	Instances []instanceView
	Now       time.Time
}

// updatePlan is what may happen THIS tick. Create/Retire are bounded by the
// budget; Repair is not.
type updatePlan struct {
	// Create is how many fresh instances to create.
	Create int

	// Retire are instances to tear down, in the order chosen.
	Retire []*types.Instance

	// Repair are broken instances to replace immediately, unbudgeted.
	Repair []*types.Instance

	// Holding is true when an update is in flight but the budget allows no
	// step this tick — the normal state between steps, waiting on a
	// replacement to become available. Distinguishes "waiting" from "done".
	Holding bool

	// Reason is one sentence for StatusMessage and events.
	Reason string
}

// planUpdate decides what may happen this tick.
func planUpdate(in updateInput) updatePlan {
	plan := updatePlan{}
	total := len(in.Instances)

	var broken, outdated, current []*instanceView
	for i := range in.Instances {
		v := &in.Instances[i]
		switch v.Class {
		case CompatBroken:
			broken = append(broken, v)
		case CompatOutdated:
			outdated = append(outdated, v)
		default:
			current = append(current, v)
		}
	}

	// The extra copy is allowed only while an update is actually in flight.
	// Outside one, the service must sit at exactly its desired scale — this
	// is what stops a finished update from leaving a permanent spare, and
	// what lets ordinary scale-down still converge.
	allowance := 0
	if len(outdated) > 0 {
		allowance = in.Params.Extra
	}

	retired := make(map[string]bool, total)
	retire := func(v *instanceView) {
		plan.Retire = append(plan.Retire, v.Instance)
		retired[v.Instance.ID] = true
	}

	// --- 1. Excess: converge toward the desired scale (§8.1) ---
	//
	// Scale-down is folded in here rather than left as a separate step. Two
	// functions both deciding "which instances die" is how the surged
	// replacement gets retired out from under an in-flight update: the old
	// standalone scale-down saw Scale+1 instances, called one of them excess,
	// and retired it with no regard for readiness. Deciding once, with the
	// allowance in hand, makes that impossible.
	if excess := total - (in.Scale + allowance); excess > 0 {
		for _, v := range excessOrder(broken, outdated, current) {
			if excess == 0 {
				break
			}
			if v.Terminating {
				continue // already going away
			}
			retire(v)
			excess--
		}
	}

	// --- 2. Repair: unbudgeted ---
	//
	// A broken instance is serving nobody, so replacing it cannot reduce
	// availability and waiting cannot help. It does not consume the create
	// budget either: its slot already exists. (One that was just retired as
	// excess is not repaired — the service no longer wants that slot.)
	for _, v := range broken {
		if !retired[v.Instance.ID] {
			plan.Repair = append(plan.Repair, v.Instance)
		}
	}

	// Recount over what survives step 1.
	live, available := 0, 0
	var liveOutdated []*instanceView
	for i := range in.Instances {
		v := &in.Instances[i]
		if retired[v.Instance.ID] {
			continue
		}
		live++
		if v.serving() {
			available++
		}
		if v.Class == CompatOutdated && !v.Terminating {
			liveOutdated = append(liveOutdated, v)
		}
	}

	// --- Recreate: everything down, then back up ---
	//
	// The pre-RUNE-042 behaviour, now opt-in. Retire every outdated instance
	// at once and create nothing until they are gone.
	if in.Params.Type == types.UpdateRecreate && len(liveOutdated) > 0 {
		for _, v := range liveOutdated {
			retire(v)
		}
		plan.Reason = fmt.Sprintf("recreate: retiring %d instance(s) before replacing them", len(liveOutdated))
		return plan
	}

	// --- 3. Retire outdated instances that are NOT available ---
	//
	// Outside the budget, deliberately. Such an instance is serving nobody,
	// so retiring it cannot reduce availability — and gating it on the budget
	// is what wedges the update: with dip=0 the retire budget is 0 whenever
	// availability is already at scale, so a stuck replacement would be held
	// forever with the fix one delete away.
	for _, v := range liveOutdated {
		if !v.serving() && !retired[v.Instance.ID] {
			retire(v)
		}
	}

	// --- 4. Retire available outdated instances, up to the budget ---
	var availableOutdated []*instanceView
	for _, v := range liveOutdated {
		if !retired[v.Instance.ID] {
			availableOutdated = append(availableOutdated, v)
		}
	}
	sortOldestFirst(availableOutdated)

	// The floor: never drop below Scale-dip serving instances. Retirements
	// already queued above were all non-available, so they have not moved
	// this number.
	retireBudget := available - (in.Scale - in.Params.Dip)
	for _, v := range availableOutdated {
		if retireBudget <= 0 {
			break
		}
		retire(v)
		retireBudget--
	}

	// --- 5. Create ---
	//
	// Capped by how many replacements could possibly be useful: one per
	// outstanding outdated instance, plus any shortfall against the desired
	// scale. Without that cap the surge allowance would have a converged
	// service create a permanent spare.
	deficit := in.Scale - live
	if deficit < 0 {
		deficit = 0
	}
	createBudget := (in.Scale + allowance) - live
	wanted := len(liveOutdated) + deficit
	if createBudget < wanted {
		wanted = createBudget
	}
	if wanted > 0 {
		plan.Create = wanted
	}

	// --- 6. Narrate ---
	plan.Holding = len(liveOutdated) > 0 && plan.Create == 0 && len(plan.Retire) == 0
	plan.Reason = describePlan(plan, len(liveOutdated))
	return plan
}

// excessOrder ranks instances for scale-down, least valuable first.
//
// Availability dominates: a serving instance is never retired while a
// non-serving one could go instead, whatever their templates. That ordering
// is what keeps scale-down inside the availability floor — retire all the
// non-serving instances first and the survivors are exactly the ones still
// carrying traffic. (Getting this wrong is subtle: the plan still respects
// the surge cap, so only the floor property catches it.)
//
// Within each availability tier, outdated goes before current — if replicas
// must be lost, lose the old template — and within a tier, oldest first. That
// last part matches the pre-RUNE-042 scale-down, which sorted newest-first and
// deleted from the tail (i.e. retired the oldest), and is the sensible rule:
// the longest-running instance is the likeliest to be holding stale state.
func excessOrder(broken, outdated, current []*instanceView) []*instanceView {
	staleNotServing, staleServing := splitByAvailability(outdated)
	freshNotServing, freshServing := splitByAvailability(current)
	for _, group := range [][]*instanceView{staleNotServing, staleServing, freshNotServing, freshServing} {
		sortOldestFirst(group)
	}

	ordered := make([]*instanceView, 0, len(broken)+len(outdated)+len(current))
	ordered = append(ordered, broken...)          // serving nobody by definition
	ordered = append(ordered, staleNotServing...) // not serving, old template
	ordered = append(ordered, freshNotServing...) // not serving, current template
	ordered = append(ordered, staleServing...)    // serving, but old template
	return append(ordered, freshServing...)       // serving and current: last resort
}

func splitByAvailability(vs []*instanceView) (notServing, serving []*instanceView) {
	for _, v := range vs {
		if v.serving() {
			serving = append(serving, v)
		} else {
			notServing = append(notServing, v)
		}
	}
	return notServing, serving
}

// sortOldestFirst orders by creation time, falling back to name so tests with
// zero timestamps stay deterministic (the same fallback the pre-RUNE-042
// scale-down used).
func sortOldestFirst(vs []*instanceView) {
	sort.SliceStable(vs, func(i, j int) bool {
		a, b := vs[i].Instance, vs[j].Instance
		if !a.CreatedAt.IsZero() || !b.CreatedAt.IsZero() {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.Name < b.Name
	})
}

// describePlan renders the one sentence operators read in `rune status`,
// `describe` and events.
func describePlan(plan updatePlan, outdated int) string {
	switch {
	case plan.Holding:
		return "waiting for replacements to become ready"
	case plan.Create > 0 && len(plan.Retire) > 0:
		return fmt.Sprintf("creating %d replacement(s), retiring %d instance(s)", plan.Create, len(plan.Retire))
	case plan.Create > 0:
		return fmt.Sprintf("creating %d replacement(s); %d instance(s) still outdated", plan.Create, outdated)
	case len(plan.Retire) > 0:
		return fmt.Sprintf("retiring %d instance(s)", len(plan.Retire))
	case len(plan.Repair) > 0:
		return fmt.Sprintf("replacing %d unhealthy instance(s)", len(plan.Repair))
	default:
		return ""
	}
}
