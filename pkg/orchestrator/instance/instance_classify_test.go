package instance

// RUNE-042 Phase 2: classification. The pre-RUNE-042 compatibility check
// returned one boolean that conflated two categories with opposite urgency —
// a crashed container and a stale template took the same "delete it" path.
// These tests pin the split, the deliberate check-order change, and the two
// behaviours that must NOT move. See design §6 and §8.5.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func classifyFixture(t *testing.T) (context.Context, *store.TestStore, *runner.TestRunner, *Controller) {
	t.Helper()
	st := store.NewTestStore()
	tr := runner.NewTestRunner()
	rm := manager.NewTestRunnerManager(nil)
	rm.SetDockerRunner(tr)
	rm.SetProcessRunner(tr)
	return context.Background(), st, tr, NewController(st, rm, log.NewLogger())
}

func classifySvc(gen int64) *types.Service {
	return &types.Service{
		ID: "svc", Name: "svc", Namespace: "default",
		Runtime:  types.RuntimeTypeContainer,
		Image:    "app:v1",
		Metadata: &types.ServiceMetadata{Generation: gen, TemplateGeneration: gen},
	}
}

func classifyInst(status types.InstanceStatus, gen int64) *types.Instance {
	created := time.Now()
	return &types.Instance{
		ID: "svc-0", Name: "svc-0", Namespace: "default",
		ServiceID: "svc", ServiceName: "svc",
		Status:                 status,
		ContainerID:            "container-abc",
		ContainerEverCreatedAt: &created,
		Metadata:               &types.InstanceMetadata{ServiceGeneration: gen, Image: "app:v1"},
	}
}

// A stale template on a HEALTHY instance is Outdated — serving fine, just old.
// This is the class the update budget will govern.
func TestClassify_StaleTemplateOnHealthyInstanceIsOutdated(t *testing.T) {
	ctx, st, tr, c := classifyFixture(t)
	svc := classifySvc(5)
	inst := classifyInst(types.InstanceStatusRunning, 3)
	require.NoError(t, st.CreateInstance(ctx, inst))
	require.NoError(t, tr.Create(ctx, inst))

	v := c.ClassifyInstance(ctx, inst, svc)
	assert.Equal(t, CompatOutdated, v.Class, "an old-but-serving instance must be Outdated, not Broken")
	assert.Contains(t, v.Reason, "service template changed")
	assert.False(t, v.Compatible())
}

// A crashed instance is Broken regardless of template — repair is unbudgeted.
func TestClassify_FailedInstanceIsBroken(t *testing.T) {
	ctx, st, tr, c := classifyFixture(t)
	svc := classifySvc(5)
	inst := classifyInst(types.InstanceStatusFailed, 5) // current template, but dead
	require.NoError(t, st.CreateInstance(ctx, inst))
	require.NoError(t, tr.Create(ctx, inst))

	v := c.ClassifyInstance(ctx, inst, svc)
	assert.Equal(t, CompatBroken, v.Class)
	assert.Contains(t, v.Reason, "failed state")
}

// THE ORDER CHANGE (§6.1): an instance that is BOTH dead in the runner AND on
// an old template must classify Broken. In the pre-RUNE-042 order the
// generation check ran first, so this reported "template changed" — which
// under a budget would count it as serving and retire it politely, when in
// fact it serves nobody.
func TestClassify_DeadInRunnerAndOutdatedIsBroken(t *testing.T) {
	ctx, st, tr, c := classifyFixture(t)
	svc := classifySvc(5)
	inst := classifyInst(types.InstanceStatusRunning, 3) // stale template
	require.NoError(t, st.CreateInstance(ctx, inst))
	require.NoError(t, tr.Create(ctx, inst))
	// ...but the runner reports it terminal.
	tr.StatusResults = map[string]types.InstanceStatus{inst.ID: types.InstanceStatusExited}

	v := c.ClassifyInstance(ctx, inst, svc)
	assert.Equal(t, CompatBroken, v.Class,
		"liveness must be evaluated before currency: a dead instance is not merely outdated")
	assert.Contains(t, v.Reason, "terminal state in the runner")
}

func TestClassify_WrongServiceIsBroken(t *testing.T) {
	ctx, st, tr, c := classifyFixture(t)
	svc := classifySvc(1)
	inst := classifyInst(types.InstanceStatusRunning, 1)
	inst.ServiceID = "someone-else"
	require.NoError(t, st.CreateInstance(ctx, inst))
	require.NoError(t, tr.Create(ctx, inst))

	v := c.ClassifyInstance(ctx, inst, svc)
	assert.Equal(t, CompatBroken, v.Class)
}

func TestClassify_MatchingInstanceIsOK(t *testing.T) {
	ctx, st, tr, c := classifyFixture(t)
	svc := classifySvc(5)
	inst := classifyInst(types.InstanceStatusRunning, 5)
	require.NoError(t, st.CreateInstance(ctx, inst))
	require.NoError(t, tr.Create(ctx, inst))

	v := c.ClassifyInstance(ctx, inst, svc)
	assert.Equal(t, CompatOK, v.Class)
	assert.True(t, v.Compatible())
}

// MUST NOT MOVE: the stuck-in-create guard. A record whose container was
// never created stays OK, or the reconciler tombstones and recreates it with
// a new UUID every tick — the churn bug the guard exists to prevent.
func TestClassify_StuckInCreateStaysOK(t *testing.T) {
	ctx, st, _, c := classifyFixture(t)
	svc := classifySvc(5)

	for i, status := range []types.InstanceStatus{types.InstanceStatusFailed, types.InstanceStatusStalled} {
		inst := classifyInst(status, 0)
		inst.ID = fmt.Sprintf("svc-stuck-%d", i)
		inst.Name = inst.ID
		inst.ContainerEverCreatedAt = nil // never got a container
		inst.ContainerID = ""
		require.NoError(t, st.CreateInstance(ctx, inst))

		v := c.ClassifyInstance(ctx, inst, svc)
		assert.Equal(t, CompatOK, v.Class,
			"a stuck-in-create %s record must hold its slot, not churn", status)
	}
}

// The boolean view must keep reporting exactly what it did before: anything
// that is not OK is "not compatible".
func TestClassify_BooleanViewUnchanged(t *testing.T) {
	ctx, st, tr, c := classifyFixture(t)
	svc := classifySvc(5)

	outdated := classifyInst(types.InstanceStatusRunning, 3)
	require.NoError(t, st.CreateInstance(ctx, outdated))
	require.NoError(t, tr.Create(ctx, outdated))
	ok, reason := c.IsInstanceCompatibleWithService(ctx, outdated, svc)
	assert.False(t, ok, "outdated must still read as incompatible to legacy callers")
	assert.NotEmpty(t, reason)

	current := classifyInst(types.InstanceStatusRunning, 5)
	current.ID, current.Name = "svc-1", "svc-1"
	require.NoError(t, st.CreateInstance(ctx, current))
	require.NoError(t, tr.Create(ctx, current))
	ok, _ = c.IsInstanceCompatibleWithService(ctx, current, svc)
	assert.True(t, ok)
}

// §6.3 / review flaw B1: UpdateInstance must NOT destroy an outdated
// instance. It is called on every reconcile for every survivor, so returning
// the recreation error for outdated ones would bypass the update budget
// entirely once Phase 4 lands.
func TestUpdateInstance_LeavesOutdatedAlone_ButFlagsBroken(t *testing.T) {
	ctx, st, tr, c := classifyFixture(t)
	svc := classifySvc(5)

	t.Run("outdated is left alone", func(t *testing.T) {
		inst := classifyInst(types.InstanceStatusRunning, 3)
		require.NoError(t, st.CreateInstance(ctx, inst))
		require.NoError(t, tr.Create(ctx, inst))

		err := c.UpdateInstance(ctx, svc, inst)
		require.NoError(t, err, "an outdated instance is the update planner's business, not UpdateInstance's")
	})

	t.Run("broken still demands recreation", func(t *testing.T) {
		inst := classifyInst(types.InstanceStatusFailed, 5)
		inst.ID, inst.Name = "svc-2", "svc-2"
		require.NoError(t, st.CreateInstance(ctx, inst))
		require.NoError(t, tr.Create(ctx, inst))

		err := c.UpdateInstance(ctx, svc, inst)
		require.Error(t, err, "a broken instance must still surface the recreation signal")
		assert.Contains(t, err.Error(), "requires recreation")
	})
}
