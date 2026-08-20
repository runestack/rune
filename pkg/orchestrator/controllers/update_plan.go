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

// routed reports whether the DATAPLANE is currently sending this instance new
// connections. republishService publishes an endpoint for any instance whose
// stored Status is Running, and nothing else, so this must mirror exactly
// that condition — no minimum-ready window, no class check.
//
// This is deliberately weaker than serving(): an instance inside its
// minimum-ready window IS taking traffic even though the budget does not yet
// count it as available. Retiring such an instance costs real requests, so
// the unbudgeted retire path must gate on routed(), not on serving().
func (v *instanceView) routed() bool {
	return v.Ready && !v.Terminating
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

	// Instances already tearing down are treated as GONE for every count.
	//
	// They cannot be retired (that would be a double teardown) and they are
	// leaving anyway, so counting them as occupying a slot only creates
	// pressure to retire somebody else. That is not hypothetical: with the
	// count including them, a scale-down computed excess against instances
	// step 1 then skipped, and retired a serving instance in place of one
	// that was already on its way out.
	//
	// Treating them as gone can transiently allow one extra container while
	// the old one finishes stopping. That is the safe direction of the two:
	// a brief overshoot costs memory, an unnecessary retirement costs
	// requests.
	total := 0
	var broken, outdated, current []*instanceView
	for i := range in.Instances {
		v := &in.Instances[i]
		if v.Terminating {
			continue
		}
		total++
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
		if retired[v.Instance.ID] || v.Terminating {
			continue // retired this tick, or already on its way out
		}
		live++
		if v.serving() {
			available++
		}
		if v.Class == CompatOutdated {
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

	// --- 3. Retire outdated instances the dataplane is NOT routing to ---
	//
	// Outside the budget, deliberately. Such an instance receives no new
	// connections, so retiring it cannot reduce availability — and gating it
	// on the budget is what wedges the update: with dip=0 the retire budget
	// is 0 whenever availability is already at scale, so a stuck replacement
	// would be held forever with the fix one delete away.
	//
	// The gate is routed(), NOT serving(). Those differ by the minimum-ready
	// window, and instances inside that window are already in the endpoint
	// set. Gating on serving() here meant that when several instances
	// reached Running together — a host reboot, a runed restart, a completed
	// restart — a template change landing within the next few seconds would
	// find none of them "serving" and retire ALL of them at once, outside
	// any budget: exactly the whole-service outage this feature exists to
	// prevent. Instances that are routed but not yet available go through
	// the budgeted step 4 instead.
	for _, v := range liveOutdated {
		if !v.routed() && !retired[v.Instance.ID] {
			retire(v)
		}
	}

	// --- 4. Retire the remaining outdated instances, up to the budget ---
	//
	// Everything still standing here is routed. Those not yet counted as
	// available (inside the minimum-ready window) go first: retiring one
	// costs the least, since the budget was never counting it.
	var budgetedOutdated []*instanceView
	for _, v := range liveOutdated {
		if !retired[v.Instance.ID] {
			budgetedOutdated = append(budgetedOutdated, v)
		}
	}
	notYetAvailable, alreadyServing := splitByAvailability(budgetedOutdated)
	sortOldestFirst(notYetAvailable)
	sortOldestFirst(alreadyServing)
	budgetedOutdated = append(notYetAvailable, alreadyServing...)

	// The floor: never drop below Scale-dip serving instances. Retirements
	// already queued above were all non-available, so they have not moved
	// this number.
	retireBudget := available - (in.Scale - in.Params.Dip)
	for _, v := range budgetedOutdated {
		if retireBudget <= 0 {
			break
		}
		retire(v)
		retireBudget--
	}

	// --- 5. Create ---
	//
	// Two limits, and the plan takes the smaller.
	//
	// The BUDGET is how many more instances may exist at all: scale plus the
	// surge allowance, less what is already here.
	//
	// The USEFUL count is how many more current-template instances the
	// service will actually end up wanting: Scale, less the ones that already
	// exist, less the ones this same plan is about to produce by repairing
	// broken instances (a repair recreates at the current template, so it
	// fills one of those slots). Without this second limit the surge
	// allowance would have a converged service carry a permanent spare, and
	// a plan that both repairs and creates would over-provision.
	currentTemplate := 0
	for i := range in.Instances {
		v := &in.Instances[i]
		if !retired[v.Instance.ID] && !v.Terminating && v.Class == CompatOK {
			currentTemplate++
		}
	}

	createBudget := (in.Scale + allowance) - live
	useful := in.Scale - currentTemplate - len(plan.Repair)
	wanted := createBudget
	if useful < wanted {
		wanted = useful
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
