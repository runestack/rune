package reconciler

// RUNE-042 Phase 4: the wiring. These are the tests that say deploys no
// longer drop traffic — the milestone's actual claim, at the reconciler
// level. See design §8.1–§8.3.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/health"
	instancectl "github.com/runestack/rune/pkg/orchestrator/instance"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// updateFixture builds a reconciler over a real instance controller, so
// classification actually consults the runner.
func updateFixture(t *testing.T) (context.Context, *store.TestStore, *runner.TestRunner, *Reconciler) {
	t.Helper()
	st := store.NewTestStore()
	tr := runner.NewTestRunner()
	rm := manager.NewTestRunnerManager(nil)
	rm.SetDockerRunner(tr)
	rm.SetProcessRunner(tr)
	logger := log.NewLogger()

	ic := instancectl.NewController(st, rm, logger)
	r := &Reconciler{
		store:              st,
		instanceController: ic,
		healthController:   health.NewFakeController(),
		logger:             logger.WithComponent("reconciler"),
	}
	return context.Background(), st, tr, r
}

func updateSvc(t *testing.T, ctx context.Context, st *store.TestStore, scale int, templateGen int64) *types.Service {
	t.Helper()
	svc := &types.Service{
		ID: "api", Name: "api", Namespace: "default",
		Image: "app:v1", Runtime: types.RuntimeTypeContainer, Scale: scale,
		Status:   types.ServiceStatusRunning,
		Ports:    []types.ServicePort{{Name: "http", Port: 8080}},
		Metadata: &types.ServiceMetadata{Generation: templateGen, TemplateGeneration: templateGen},
	}
	require.NoError(t, st.CreateService(ctx, svc))
	return svc
}

func seedInstance(t *testing.T, ctx context.Context, st *store.TestStore, tr *runner.TestRunner, svc *types.Service, name string, gen int64, age time.Duration) *types.Instance {
	t.Helper()
	created := time.Now().Add(-age)
	inst := &types.Instance{
		ID: name, Name: name, Namespace: "default",
		ServiceID: svc.ID, ServiceName: svc.Name,
		Status:                 types.InstanceStatusRunning,
		ContainerID:            "c-" + name,
		ContainerEverCreatedAt: &created,
		CreatedAt:              created,
		LastTransitionAt:       &created,
		Metadata:               &types.InstanceMetadata{ServiceGeneration: gen, Image: svc.Image},
	}
	require.NoError(t, st.CreateInstance(ctx, inst))
	require.NoError(t, tr.Create(ctx, inst))
	tr.StatusResults[inst.ID] = types.InstanceStatusRunning
	return inst
}

func liveInstances(t *testing.T, ctx context.Context, st *store.TestStore) []types.Instance {
	t.Helper()
	var all []types.Instance
	require.NoError(t, st.List(ctx, types.ResourceTypeInstance, "default", &all))
	var live []types.Instance
	for _, i := range all {
		if i.Status != types.InstanceStatusDeleted {
			live = append(live, i)
		}
	}
	return live
}

// THE MILESTONE CLAIM: a template change must not take every instance down at
// once. Before RUNE-042 this deleted all four in one pass.
func TestReconcile_TemplateChangeDoesNotTakeEveryoneDown(t *testing.T) {
	ctx, st, tr, r := updateFixture(t)
	svc := updateSvc(t, ctx, st, 4, 1)
	for i := 0; i < 4; i++ {
		seedInstance(t, ctx, st, tr, svc, fmt.Sprintf("api-old-%d", i), 1, time.Duration(40-i*10)*time.Second)
	}

	// A cast stamps a new template generation.
	svc.Metadata.Generation = 2
	svc.Metadata.TemplateGeneration = 2
	require.NoError(t, st.Update(ctx, types.ResourceTypeService, "default", svc.Name, svc))

	require.NoError(t, r.reconcileService(ctx, svc))

	live := liveInstances(t, ctx, st)
	serving := 0
	for _, i := range live {
		if i.Status == types.InstanceStatusRunning && i.Metadata != nil && i.Metadata.ServiceGeneration == 1 {
			serving++
		}
	}
	assert.Equal(t, 4, serving,
		"all four old instances must still be serving after one tick — the surge replacement comes first")
	assert.Len(t, live, 5, "exactly one extra instance (the surge allowance) may exist")
}

// Availability must never dip below scale across a whole convergence, driven
// tick by tick the way the reconciler really runs.
func TestReconcile_UpdateConvergesWithoutDroppingBelowScale(t *testing.T) {
	ctx, st, tr, r := updateFixture(t)
	const scale = 3
	svc := updateSvc(t, ctx, st, scale, 1)
	for i := 0; i < scale; i++ {
		seedInstance(t, ctx, st, tr, svc, fmt.Sprintf("api-old-%d", i), 1, time.Duration(30-i*10)*time.Second)
	}

	svc.Metadata.Generation = 2
	svc.Metadata.TemplateGeneration = 2
	require.NoError(t, st.Update(ctx, types.ResourceTypeService, "default", svc.Name, svc))

	for tick := 0; tick < 12; tick++ {
		var fresh types.Service
		require.NoError(t, st.Get(ctx, types.ResourceTypeService, "default", "api", &fresh))
		require.NoError(t, r.reconcileService(ctx, &fresh))

		// Newly created instances start Pending in the fake runner; promote
		// them so the next tick sees a serving replacement, as the health
		// controller would.
		live := liveInstances(t, ctx, st)

		// Count what was serving DURING the reconcile just executed — before
		// standing in for the health controller below. Counting afterwards
		// would credit instances that were not yet serving when the plan
		// retired their predecessors, which is exactly the dip this test
		// exists to catch.
		servingNow := 0
		for i := range live {
			if live[i].Status == types.InstanceStatusRunning {
				servingNow++
			}
		}

		for i := range live {
			inst := live[i]

			// Stand in for the health controller: a fresh container reaches
			// Running.
			if inst.Status == types.InstanceStatusPending || inst.Status == types.InstanceStatusStarting {
				inst.Status = types.InstanceStatusRunning
				tr.StatusResults[inst.ID] = types.InstanceStatusRunning
			}

			// Model the wall-clock gap between reconcile ticks. This service
			// has no readiness probe, so minReady is 5s; without backdating,
			// every tick executes inside the same millisecond and no
			// replacement ever becomes available — the gate working
			// correctly, but not what this test is about.
			if inst.Status == types.InstanceStatusRunning {
				settled := time.Now().Add(-time.Minute)
				if inst.LastTransitionAt == nil || inst.LastTransitionAt.After(settled) {
					inst.LastTransitionAt = &settled
				}
			}
			require.NoError(t, st.Update(ctx, types.ResourceTypeInstance, "default", inst.ID, &inst))
		}
		assert.GreaterOrEqualf(t, servingNow, scale,
			"tick %d: availability fell to %d, below the floor of %d — this service is "+
				"surge-capable, so dip is 0 and the floor is the full scale",
			tick, servingNow, scale)
	}

	// Converged: exactly `scale` instances, all on the new template.
	live := liveInstances(t, ctx, st)
	assert.Len(t, live, scale, "the update must converge back to exactly the desired scale")
	for _, i := range live {
		require.NotNil(t, i.Metadata)
		assert.Equal(t, int64(2), i.Metadata.ServiceGeneration, "every survivor must be on the new template")
	}
}

// The status rules that break under surge (§8.2).
func TestReconcile_StatusDuringUpdate(t *testing.T) {
	ctx, st, tr, r := updateFixture(t)
	svc := updateSvc(t, ctx, st, 2, 1)
	seedInstance(t, ctx, st, tr, svc, "api-a", 1, 30*time.Second)
	seedInstance(t, ctx, st, tr, svc, "api-b", 1, 20*time.Second)

	svc.Metadata.Generation = 2
	svc.Metadata.TemplateGeneration = 2
	require.NoError(t, st.Update(ctx, types.ResourceTypeService, "default", svc.Name, svc))
	require.NoError(t, r.reconcileService(ctx, svc))

	var got types.Service
	require.NoError(t, st.Get(ctx, types.ResourceTypeService, "default", "api", &got))

	assert.Equal(t, types.ServiceStatusDeploying, got.Status,
		"a surged update must not report Stopping just because the count exceeds scale")
	assert.Equal(t, types.ServiceReasonUpdating, got.StatusReason)

	require.NotNil(t, got.Update, "an in-flight update must publish its progress")
	assert.Equal(t, 2, got.Update.Desired)
	assert.Equal(t, 2, got.Update.Outdated)
	assert.Equal(t, int64(2), got.Update.TemplateGeneration)
	assert.NotEmpty(t, got.Update.Message, "operators need a sentence, not just counters")
}

// Update is cleared when the update converges, so Verify and the CLI spinner
// never see a completed-but-still-present block.
func TestReconcile_UpdateStatusClearedOnCompletion(t *testing.T) {
	ctx, st, tr, r := updateFixture(t)
	svc := updateSvc(t, ctx, st, 1, 2)
	seedInstance(t, ctx, st, tr, svc, "api-current", 2, 10*time.Second)

	require.NoError(t, r.reconcileService(ctx, svc))

	var got types.Service
	require.NoError(t, st.Get(ctx, types.ResourceTypeService, "default", "api", &got))
	assert.Nil(t, got.Update, "no outdated instances means no update in flight")
	assert.Equal(t, types.ServiceStatusRunning, got.Status)
}

// A scale-down still converges through the planner now that the standalone
// pass no longer runs for stateless services.
func TestReconcile_ScaleDownStillConverges(t *testing.T) {
	ctx, st, tr, r := updateFixture(t)
	svc := updateSvc(t, ctx, st, 1, 1)
	seedInstance(t, ctx, st, tr, svc, "api-a", 1, 30*time.Second)
	seedInstance(t, ctx, st, tr, svc, "api-b", 1, 20*time.Second)
	seedInstance(t, ctx, st, tr, svc, "api-c", 1, 10*time.Second)

	require.NoError(t, r.reconcileService(ctx, svc))
	assert.Len(t, liveInstances(t, ctx, st), 1, "scale-down must still shed the excess")
}

// A recreate-strategy service keeps the pre-RUNE-042 behaviour exactly.
func TestReconcile_RecreateTakesAllDownAtOnce(t *testing.T) {
	ctx, st, tr, r := updateFixture(t)
	svc := updateSvc(t, ctx, st, 3, 1)
	svc.UpdateStrategy = &types.UpdateStrategy{Type: types.UpdateRecreate}
	for i := 0; i < 3; i++ {
		seedInstance(t, ctx, st, tr, svc, fmt.Sprintf("api-old-%d", i), 1, time.Duration(30-i*10)*time.Second)
	}
	svc.Metadata.Generation = 2
	svc.Metadata.TemplateGeneration = 2
	require.NoError(t, st.Update(ctx, types.ResourceTypeService, "default", svc.Name, svc))

	require.NoError(t, r.reconcileService(ctx, svc))

	// Assert the count explicitly. Iterating the survivors alone made this
	// vacuous: recreate leaves the set empty, so the loop body never ran and
	// the test would have passed even if recreate did nothing.
	live := liveInstances(t, ctx, st)
	assert.Empty(t, live, "recreate must retire every outdated instance in one pass before creating any")
	for _, i := range live {
		require.NotNil(t, i.Metadata)
		assert.NotEqual(t, int64(1), i.Metadata.ServiceGeneration)
	}
}

// M1 regression: instances that are Running (and therefore in the dataplane
// endpoint set) but still inside the minimum-ready window must NOT be
// retired unbudgeted. Several instances reaching Running together is routine
// — a host reboot, a runed restart, a completed `rune restart` — and a
// template change landing seconds later used to find none of them "serving"
// and retire ALL of them at once.
func TestReconcile_TemplateChangeJustAfterRestartKeepsServing(t *testing.T) {
	ctx, st, tr, r := updateFixture(t)
	svc := updateSvc(t, ctx, st, 3, 1)

	// All three came up a moment ago: Running, in the endpoint set, but
	// within the 5s minReady window (no readiness probe on this service).
	justNow := time.Now()
	for i := 0; i < 3; i++ {
		inst := seedInstance(t, ctx, st, tr, svc, fmt.Sprintf("api-%d", i), 1, time.Second)
		inst.LastTransitionAt = &justNow
		require.NoError(t, st.Update(ctx, types.ResourceTypeInstance, "default", inst.ID, inst))
	}

	svc.Metadata.Generation = 2
	svc.Metadata.TemplateGeneration = 2
	require.NoError(t, st.Update(ctx, types.ResourceTypeService, "default", svc.Name, svc))

	require.NoError(t, r.reconcileService(ctx, svc))

	serving := 0
	for _, i := range liveInstances(t, ctx, st) {
		if i.Status == types.InstanceStatusRunning && i.Metadata != nil && i.Metadata.ServiceGeneration == 1 {
			serving++
		}
	}
	assert.Equal(t, 3, serving,
		"instances inside the minimum-ready window are still taking traffic and must not be retired unbudgeted")
}

// H1 regression: a service whose dependency goes unready must still honour a
// scale-down. The dependency gate blocks CREATION, not the whole pass.
func TestReconcile_ScaleDownWorksWhileDependencyUnready(t *testing.T) {
	ctx, st, tr, r := updateFixture(t)

	// A dependency that exists but has no running instances.
	dep := &types.Service{
		ID: "db", Name: "db", Namespace: "default", Image: "db:v1",
		Runtime: types.RuntimeTypeContainer, Scale: 1,
		Metadata: &types.ServiceMetadata{Generation: 1, TemplateGeneration: 1},
	}
	require.NoError(t, st.CreateService(ctx, dep))

	svc := updateSvc(t, ctx, st, 1, 1)
	svc.Dependencies = []types.DependencyRef{{Service: "db", Namespace: "default"}}
	require.NoError(t, st.Update(ctx, types.ResourceTypeService, "default", svc.Name, svc))
	for i := 0; i < 3; i++ {
		seedInstance(t, ctx, st, tr, svc, fmt.Sprintf("api-%d", i), 1, time.Duration(30-i*10)*time.Second)
	}

	require.NoError(t, r.reconcileService(ctx, svc))

	assert.Len(t, liveInstances(t, ctx, st), 1,
		"an unready dependency must not stop `rune scale` from shedding instances")
}

// H2 regression: a record stranded in Terminating (runed killed mid-teardown)
// must be re-driven, not left holding a slot forever.
func TestReconcile_ReapsInstanceStrandedInTerminating(t *testing.T) {
	ctx, st, tr, r := updateFixture(t)
	svc := updateSvc(t, ctx, st, 2, 1)
	seedInstance(t, ctx, st, tr, svc, "api-ok", 1, 30*time.Second)

	stranded := seedInstance(t, ctx, st, tr, svc, "api-stranded", 1, 30*time.Second)
	stranded.Status = types.InstanceStatusTerminating
	stranded.UpdatedAt = time.Now().Add(-10 * time.Minute) // long abandoned
	require.NoError(t, st.Update(ctx, types.ResourceTypeInstance, "default", stranded.ID, stranded))

	require.NoError(t, r.reconcileService(ctx, svc))

	got, err := st.GetInstanceByID(ctx, "default", "api-stranded")
	require.NoError(t, err)
	assert.Equal(t, types.InstanceStatusDeleted, got.Status,
		"an abandoned teardown must be finished, or the slot is held forever")
}

// Stall detection: an update whose replacements never become ready must
// eventually report Failed/UpdateStalled — this is what lets `cast --atomic`
// roll a wedged update back.
func TestReconcile_StalledUpdateSurfacesAsFailed(t *testing.T) {
	ctx, st, tr, r := updateFixture(t)
	svc := updateSvc(t, ctx, st, 2, 2)
	// One old instance still serving, and BOTH replacement slots already
	// filled by instances that never became ready — the surge cap is spent,
	// so no create is possible, and with dip=0 no retire is either. A
	// genuinely stuck update, which is the only state that should stall.
	seedInstance(t, ctx, st, tr, svc, "api-old", 1, 60*time.Second)
	stuckA := seedInstance(t, ctx, st, tr, svc, "api-new-a", 2, 30*time.Second)
	stuckB := seedInstance(t, ctx, st, tr, svc, "api-new-b", 2, 30*time.Second)
	for _, stuck := range []*types.Instance{stuckA, stuckB} {
		stuck.Status = types.InstanceStatusStarting
		require.NoError(t, st.Update(ctx, types.ResourceTypeInstance, "default", stuck.ID, stuck))
	}

	// ...and it has been in that state for longer than the stall deadline.
	long := time.Now().Add(-2 * types.UpdateStallSeconds * time.Second)
	svc.Update = &types.UpdateStatus{
		TemplateGeneration: 2,
		Desired:            2,
		Updated:            2,
		UpdatedReady:       0,
		Available:          1,
		Outdated:           1,
		StartedAt:          long,
		LastProgressAt:     long,
		// A real persisted block always carries its high-water marks, which
		// is what makes the oscillation of a crash-loop stop counting as
		// progress. Without them this fixture would look like a brand-new
		// update on its first tick.
		PeakUpdated:      2,
		PeakUpdatedReady: 0,
		PeakAvailable:    1,
	}
	require.NoError(t, st.Update(ctx, types.ResourceTypeService, "default", svc.Name, svc))

	var fresh types.Service
	require.NoError(t, st.Get(ctx, types.ResourceTypeService, "default", "api", &fresh))
	require.NoError(t, r.reconcileService(ctx, &fresh))

	var got types.Service
	require.NoError(t, st.Get(ctx, types.ResourceTypeService, "default", "api", &got))
	assert.Equal(t, types.ServiceStatusFailed, got.Status,
		"an update that stops progressing must surface as Failed so --atomic can revert it")
	assert.Equal(t, types.ServiceReasonUpdateStalled, got.StatusReason)
}
