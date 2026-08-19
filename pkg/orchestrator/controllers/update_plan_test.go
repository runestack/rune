package controllers

// RUNE-042 Phase 3: the planner's table and property tests. This is where the
// design's real risk gets retired — before any reconciler wiring exists.

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fixtures -------------------------------------------------------------

var planBase = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

// view builds an instanceView. age orders instances for retirement (larger =
// older), so callers can express "retire the oldest" expectations directly.
func view(name string, class CompatClass, available bool, ageSeconds int) instanceView {
	inst := &types.Instance{
		ID:        name,
		Name:      name,
		CreatedAt: planBase.Add(-time.Duration(ageSeconds) * time.Second),
		Status:    types.InstanceStatusRunning,
	}
	if !available {
		inst.Status = types.InstanceStatusStarting
	}
	return instanceView{
		Instance:  inst,
		Class:     class,
		Ready:     available,
		Available: available,
	}
}

// surgeable is the derived params for a service that can run two copies.
func surgeable() types.UpdateParams {
	return types.UpdateParams{Type: types.UpdateRolling, Extra: 1, Dip: 0}
}

// exclusive is the derived params for one that cannot (claimTemplate volume,
// hostPort, or process runtime).
func exclusive() types.UpdateParams {
	return types.UpdateParams{Type: types.UpdateRolling, Extra: 0, Dip: 1}
}

func retireNames(p updatePlan) []string {
	out := make([]string, 0, len(p.Retire))
	for _, i := range p.Retire {
		out = append(out, i.Name)
	}
	return out
}

// --- the design's worked example (§7.2) -----------------------------------

// A surge-capable scale-4 service updating from a template change must never
// drop below 4 serving instances, and must converge.
func TestPlanUpdate_WorkedExample(t *testing.T) {
	// Tick 1: four outdated and available. One spare allowed, no dip.
	insts := []instanceView{
		view("A", CompatOutdated, true, 40),
		view("B", CompatOutdated, true, 30),
		view("C", CompatOutdated, true, 20),
		view("D", CompatOutdated, true, 10),
	}
	p := planUpdate(updateInput{Scale: 4, Params: surgeable(), Instances: insts, Now: planBase})
	assert.Equal(t, 1, p.Create, "tick 1: create the spare")
	assert.Empty(t, p.Retire, "tick 1: nothing may retire while availability is exactly at scale")
	assert.False(t, p.Holding)

	// Tick 2: the replacement exists but is not yet ready.
	insts = append(insts, view("E", CompatOK, false, 0))
	p = planUpdate(updateInput{Scale: 4, Params: surgeable(), Instances: insts, Now: planBase})
	assert.Equal(t, 0, p.Create, "tick 2: the surge slot is taken")
	assert.Empty(t, p.Retire, "tick 2: E is not serving yet, so nothing may go")
	assert.True(t, p.Holding, "tick 2: this is the waiting state")

	// Tick 3: E becomes available -> the oldest outdated instance may retire.
	insts[4] = view("E", CompatOK, true, 0)
	p = planUpdate(updateInput{Scale: 4, Params: surgeable(), Instances: insts, Now: planBase})
	assert.Equal(t, []string{"A"}, retireNames(p), "tick 3: retire the oldest outdated instance")
	assert.Equal(t, 0, p.Create)

	// Tick 4: A is gone -> the surge slot frees up again.
	insts = []instanceView{
		view("B", CompatOutdated, true, 30),
		view("C", CompatOutdated, true, 20),
		view("D", CompatOutdated, true, 10),
		view("E", CompatOK, true, 0),
	}
	p = planUpdate(updateInput{Scale: 4, Params: surgeable(), Instances: insts, Now: planBase})
	assert.Equal(t, 1, p.Create, "tick 4: create the next replacement")
	assert.Empty(t, p.Retire)
}

// A converged service must be completely quiet — no spare left behind.
func TestPlanUpdate_ConvergedIsQuiet(t *testing.T) {
	insts := []instanceView{
		view("a", CompatOK, true, 30),
		view("b", CompatOK, true, 20),
	}
	p := planUpdate(updateInput{Scale: 2, Params: surgeable(), Instances: insts, Now: planBase})
	assert.Equal(t, 0, p.Create, "the surge allowance must not create a permanent spare")
	assert.Empty(t, p.Retire)
	assert.Empty(t, p.Repair)
	assert.False(t, p.Holding, "no update in flight means not holding")
}

// --- the wedge the design calls out (§7.1 rule 3) -------------------------

// The double-push: an operator casts a fix while the previous replacement is
// still stuck Starting. With dip=0 the retire budget is 0, so gating the
// not-available instance on the budget would hold until the stall deadline
// with the fix one delete away.
func TestPlanUpdate_NotAvailableOutdatedRetiresOutsideBudget(t *testing.T) {
	insts := []instanceView{
		view("old", CompatOutdated, true, 60),
		view("stuck", CompatOutdated, false, 30), // never became ready
	}
	p := planUpdate(updateInput{Scale: 2, Params: exclusive(), Instances: insts, Now: planBase})

	assert.Contains(t, retireNames(p), "stuck",
		"an outdated instance that serves nobody must be retirable regardless of the budget")
	assert.NotContains(t, retireNames(p), "old",
		"the serving instance must still be protected by the budget")
}

// The same rule must not let the floor be breached: retiring non-available
// instances is free precisely because they were never counted as serving.
func TestPlanUpdate_FloorHoldsWhileRetiringUnavailable(t *testing.T) {
	insts := []instanceView{
		view("s1", CompatOutdated, false, 40),
		view("s2", CompatOutdated, false, 30),
		view("ok", CompatOutdated, true, 20),
	}
	p := planUpdate(updateInput{Scale: 3, Params: surgeable(), Instances: insts, Now: planBase})
	assert.ElementsMatch(t, []string{"s1", "s2"}, retireNames(p))
	assert.NotContains(t, retireNames(p), "ok", "the only serving instance must survive with dip=0")
}

// --- repair is unbudgeted -------------------------------------------------

func TestPlanUpdate_RepairIsUnbudgeted(t *testing.T) {
	insts := []instanceView{
		view("broken", CompatBroken, false, 40),
		view("a", CompatOK, true, 30),
		view("b", CompatOK, true, 20),
	}
	p := planUpdate(updateInput{Scale: 3, Params: surgeable(), Instances: insts, Now: planBase})

	require.Len(t, p.Repair, 1)
	assert.Equal(t, "broken", p.Repair[0].Name, "crash recovery must never wait on the update budget")
	assert.Empty(t, p.Retire)
}

// A broken instance must not starve behind an exhausted update budget.
func TestPlanUpdate_RepairProceedsDuringUpdate(t *testing.T) {
	insts := []instanceView{
		view("old1", CompatOutdated, true, 50),
		view("old2", CompatOutdated, true, 40),
		view("broken", CompatBroken, false, 30),
		view("new", CompatOK, false, 10), // surge slot taken, not ready
	}
	p := planUpdate(updateInput{Scale: 3, Params: surgeable(), Instances: insts, Now: planBase})

	require.Len(t, p.Repair, 1)
	assert.Equal(t, "broken", p.Repair[0].Name)
}

// A plan that both repairs and creates must not over-provision: a repair
// recreates at the current template, so it fills one of the slots the create
// step would otherwise ask for.
func TestPlanUpdate_RepairAndCreateDoNotOverProvision(t *testing.T) {
	insts := []instanceView{
		view("broken", CompatBroken, false, 40),
		view("ok", CompatOK, true, 30),
	}
	p := planUpdate(updateInput{Scale: 2, Params: surgeable(), Instances: insts, Now: planBase})

	require.Len(t, p.Repair, 1)
	assert.Equal(t, 0, p.Create,
		"the repair already produces the second current-template instance")

	// After the repair lands, the service is at scale and quiet.
	settled := []instanceView{
		view("ok", CompatOK, true, 30),
		view("repaired", CompatOK, true, 0),
	}
	p = planUpdate(updateInput{Scale: 2, Params: surgeable(), Instances: settled, Now: planBase})
	assert.Equal(t, 0, p.Create)
	assert.Empty(t, p.Retire)
}

// --- recreate -------------------------------------------------------------

func TestPlanUpdate_RecreateTakesEverythingDownFirst(t *testing.T) {
	params := types.UpdateParams{Type: types.UpdateRecreate, Extra: 0, Dip: 3}
	insts := []instanceView{
		view("a", CompatOutdated, true, 30),
		view("b", CompatOutdated, true, 20),
		view("c", CompatOutdated, true, 10),
	}
	p := planUpdate(updateInput{Scale: 3, Params: params, Instances: insts, Now: planBase})

	assert.ElementsMatch(t, []string{"a", "b", "c"}, retireNames(p), "recreate retires all at once")
	assert.Equal(t, 0, p.Create, "and creates nothing until they are gone")

	// Once they are gone, it refills.
	p = planUpdate(updateInput{Scale: 3, Params: params, Instances: nil, Now: planBase})
	assert.Equal(t, 3, p.Create)
}

// --- scale-down folded in (§8.1) -----------------------------------------

// The collision this fold exists to prevent: with a standalone scale-down,
// a surged replacement (or the instance it replaces) gets retired mid-update
// with no regard for readiness.
func TestPlanUpdate_ScaleDownDoesNotFightTheSurge(t *testing.T) {
	insts := []instanceView{
		view("A", CompatOutdated, true, 40),
		view("B", CompatOutdated, true, 30),
		view("C", CompatOutdated, true, 20),
		view("D", CompatOutdated, true, 10),
		view("E", CompatOK, false, 0), // the surged replacement, still starting
	}
	p := planUpdate(updateInput{Scale: 4, Params: surgeable(), Instances: insts, Now: planBase})

	assert.Empty(t, p.Retire,
		"five instances at scale 4 is the surge allowance, not excess — nothing may be retired")
	assert.True(t, p.Holding)
}

// A genuine scale-down still converges, retiring the oldest — the
// pre-RUNE-042 behaviour (which sorted newest-first and deleted the tail).
func TestPlanUpdate_GenuineScaleDownRetiresOldest(t *testing.T) {
	insts := []instanceView{
		view("oldest", CompatOK, true, 40),
		view("middle", CompatOK, true, 30),
		view("newest", CompatOK, true, 10),
	}
	p := planUpdate(updateInput{Scale: 1, Params: surgeable(), Instances: insts, Now: planBase})
	assert.Equal(t, []string{"oldest", "middle"}, retireNames(p))
}

// Scaling down mid-update should shed the old template preferentially.
func TestPlanUpdate_ScaleDownPrefersOutdatedThenBroken(t *testing.T) {
	insts := []instanceView{
		view("current", CompatOK, true, 40),
		view("outdated", CompatOutdated, true, 30),
		view("broken", CompatBroken, false, 20),
	}
	p := planUpdate(updateInput{Scale: 1, Params: surgeable(), Instances: insts, Now: planBase})

	require.Len(t, p.Retire, 2)
	assert.Equal(t, "broken", p.Retire[0].Name, "shed the instance serving nobody first")
	assert.Equal(t, "outdated", p.Retire[1].Name, "then the old template")
	assert.Empty(t, p.Repair, "an instance retired as excess must not also be repaired")
}

// --- scale-up and deficit -------------------------------------------------

func TestPlanUpdate_ScaleUpCreatesDeficit(t *testing.T) {
	insts := []instanceView{view("a", CompatOK, true, 30)}
	p := planUpdate(updateInput{Scale: 4, Params: surgeable(), Instances: insts, Now: planBase})
	assert.Equal(t, 3, p.Create, "a scale-up creates the shortfall")
	assert.Empty(t, p.Retire)
}

func TestPlanUpdate_EmptyServiceCreatesToScale(t *testing.T) {
	p := planUpdate(updateInput{Scale: 3, Params: surgeable(), Instances: nil, Now: planBase})
	assert.Equal(t, 3, p.Create)
	assert.False(t, p.Holding)
}

func TestPlanUpdate_ScaleZeroRetiresEverything(t *testing.T) {
	insts := []instanceView{
		view("a", CompatOK, true, 30),
		view("b", CompatOK, true, 20),
	}
	p := planUpdate(updateInput{Scale: 0, Params: surgeable(), Instances: insts, Now: planBase})
	assert.Len(t, p.Retire, 2)
	assert.Equal(t, 0, p.Create)
}

// --- exclusive services (extra=0, dip=1) ---------------------------------

func TestPlanUpdate_ExclusiveServiceRetiresBeforeCreating(t *testing.T) {
	insts := []instanceView{
		view("a", CompatOutdated, true, 30),
		view("b", CompatOutdated, true, 20),
	}
	p := planUpdate(updateInput{Scale: 2, Params: exclusive(), Instances: insts, Now: planBase})
	assert.Equal(t, []string{"a"}, retireNames(p), "one at a time, oldest first")
	assert.Equal(t, 0, p.Create, "no room to create until it is gone")

	// Next tick: the slot is free.
	insts = []instanceView{view("b", CompatOutdated, true, 20)}
	p = planUpdate(updateInput{Scale: 2, Params: exclusive(), Instances: insts, Now: planBase})
	assert.Equal(t, 1, p.Create)
}

// --- minReady -------------------------------------------------------------

// newInstanceView is where minReady is applied; a Running instance that has
// not held the state long enough is not yet "serving".
func TestNewInstanceView_MinReadyGate(t *testing.T) {
	justNow := planBase.Add(-1 * time.Second)
	old := planBase.Add(-30 * time.Second)

	fresh := &types.Instance{ID: "fresh", Status: types.InstanceStatusRunning, LastTransitionAt: &justNow}
	settled := &types.Instance{ID: "settled", Status: types.InstanceStatusRunning, LastTransitionAt: &old}

	v := newInstanceView(fresh, CompatOK, 5*time.Second, planBase)
	assert.True(t, v.Ready)
	assert.False(t, v.Available, "Running for 1s with a 5s window is not yet serving")

	v = newInstanceView(settled, CompatOK, 5*time.Second, planBase)
	assert.True(t, v.Available)

	// Pre-upgrade instances have no timestamp and must not wedge the update.
	legacy := &types.Instance{ID: "legacy", Status: types.InstanceStatusRunning}
	v = newInstanceView(legacy, CompatOK, 5*time.Second, planBase)
	assert.True(t, v.Available, "a nil LastTransitionAt must count as available")
}

// --- the property that matters -------------------------------------------

// The safety claim of the whole feature, checked mechanically rather than by
// example: no plan may take availability below Scale-dip, and no plan may
// push the instance count above Scale+extra.
//
// This is the property that catches the scale-down collision — a surge-cap
// check alone would not, because retiring a serving instance mid-surge keeps
// the total within cap while breaching the floor.
func TestPlanUpdate_Property_NeverBreachesFloorOrCap(t *testing.T) {
	rng := rand.New(rand.NewSource(20260819))
	classes := []CompatClass{CompatOK, CompatOutdated, CompatBroken}

	for trial := 0; trial < 3000; trial++ {
		scale := rng.Intn(6) // 0..5
		params := surgeable()
		if rng.Intn(2) == 0 {
			params = exclusive()
		}
		if rng.Intn(6) == 0 {
			params = types.UpdateParams{Type: types.UpdateRecreate, Extra: 0, Dip: maxInt(scale, 1)}
		}

		n := rng.Intn(8)
		insts := make([]instanceView, 0, n)
		for i := 0; i < n; i++ {
			insts = append(insts, view(
				fmt.Sprintf("i%d", i),
				classes[rng.Intn(len(classes))],
				rng.Intn(2) == 0,
				rng.Intn(100),
			))
		}

		in := updateInput{Scale: scale, Params: params, Instances: insts, Now: planBase}
		before := snapshotCounts(insts)
		p := planUpdate(in)

		// Which serving instances does this plan remove?
		retiredServing := 0
		retiredIDs := map[string]bool{}
		for _, r := range p.Retire {
			retiredIDs[r.ID] = true
		}
		for i := range insts {
			v := &insts[i]
			if retiredIDs[v.Instance.ID] && v.serving() {
				retiredServing++
			}
		}

		// FLOOR: availability after this plan must not fall below Scale-dip
		// — unless it was already below (the planner cannot conjure serving
		// instances, only avoid removing them).
		floor := scale - params.Dip
		if floor < 0 {
			floor = 0
		}
		after := before.available - retiredServing
		if before.available >= floor {
			assert.GreaterOrEqualf(t, after, floor,
				"trial %d: plan drove availability %d -> %d, below the floor %d (scale=%d dip=%d extra=%d type=%s)\n  in:  %s\n  plan: create=%d retire=%v repair=%d",
				trial, before.available, after, floor, scale, params.Dip, params.Extra, params.Type,
				dumpViews(insts), p.Create, retireNames(p), len(p.Repair))
		}

		// CAP: the instance count after this plan must not exceed Scale+extra.
		// The allowance only applies while an update is in flight.
		allowance := 0
		if before.outdated > 0 {
			allowance = params.Extra
		}
		total := len(insts) - len(p.Retire) + p.Create
		assert.LessOrEqualf(t, total, scale+allowance,
			"trial %d: plan drove the instance count to %d, above the cap %d (scale=%d extra=%d)",
			trial, total, scale+allowance, scale, params.Extra)

		// LIVENESS: an update in flight must always be making progress or
		// explicitly holding — never silently stuck with budget to spare.
		if before.outdated > 0 && p.Create == 0 && len(p.Retire) == 0 {
			assert.Truef(t, p.Holding, "trial %d: an idle tick during an update must report Holding", trial)
		}

		// No instance may be both retired and repaired.
		for _, r := range p.Repair {
			assert.Falsef(t, retiredIDs[r.ID], "trial %d: %s is both retired and repaired", trial, r.ID)
		}
	}
}

type planCounts struct{ available, outdated int }

func snapshotCounts(vs []instanceView) planCounts {
	var c planCounts
	for i := range vs {
		v := &vs[i]
		if v.serving() {
			c.available++
		}
		if v.Class == CompatOutdated {
			c.outdated++
		}
	}
	return c
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func dumpViews(vs []instanceView) string {
	out := ""
	for _, v := range vs {
		cls := "OK"
		switch v.Class {
		case CompatOutdated:
			cls = "OUTDATED"
		case CompatBroken:
			cls = "BROKEN"
		}
		out += fmt.Sprintf("[%s %s avail=%t] ", v.Instance.Name, cls, v.Available)
	}
	return out
}
