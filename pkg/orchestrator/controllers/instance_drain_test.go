package controllers

// RUNE-042 Phase 0 (drain fix): every teardown must withdraw an instance
// from the dataplane endpoint set BEFORE stopping its container, and give
// in-flight work a drain window in between. Whole-service teardowns take
// one shared window instead of one per instance. See
// _docs/designs/RUNE-042-Rolling-Updates.md §4.

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// seqLog records ordered events across the endpoint publisher and the
// runner so tests can assert withdraw-happens-before-stop.
type seqLog struct {
	mu     sync.Mutex
	events []string
}

func (l *seqLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *seqLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.events))
	copy(out, l.events)
	return out
}

func (l *seqLog) indexOf(e string) int {
	for i, ev := range l.snapshot() {
		if ev == e {
			return i
		}
	}
	return -1
}

// seqPublisher is an EndpointPublisher that logs each service publish with
// its endpoint count ("publish:0" = the set no longer routes anywhere).
type seqPublisher struct{ log *seqLog }

func (p *seqPublisher) PublishService(_ context.Context, _ *types.Service, eps []types.Endpoint) error {
	p.log.add(fmt.Sprintf("publish:%d", len(eps)))
	return nil
}

func (p *seqPublisher) PublishLocalInstances(context.Context, string, map[string]types.InstanceIdentity) error {
	return nil
}

// setupDrainFixture builds a controller with a recording publisher, a short
// drain window, a service WITH ports (so the drain gate is armed), and n
// Running instances present in both store and runner.
func setupDrainFixture(t *testing.T, n int, drain time.Duration) (context.Context, *store.TestStore, *runner.TestRunner, *instanceController, *seqLog, []*types.Instance) {
	t.Helper()
	ctx := context.Background()
	st := store.NewTestStore()
	tr := runner.NewTestRunner()
	rm := manager.NewTestRunnerManager(nil)
	rm.SetDockerRunner(tr)
	rm.SetProcessRunner(tr)

	c := NewInstanceController(st, rm, log.NewLogger()).(*instanceController)
	c.drainWindow = drain
	lg := &seqLog{}
	c.SetEndpointPublisher(&seqPublisher{log: lg}, "node-test")

	svc := &types.Service{
		ID: "drain-svc", Name: "drain-svc", Namespace: "default",
		Runtime:       "container",
		RestartPolicy: types.RestartPolicyAlways,
		Scale:         n,
		Ports:         []types.ServicePort{{Name: "http", Port: 8080}},
	}
	require.NoError(t, st.CreateService(ctx, svc))

	insts := make([]*types.Instance, 0, n)
	for i := 0; i < n; i++ {
		now := time.Now()
		inst := &types.Instance{
			ID:                     fmt.Sprintf("drain-svc-%d", i),
			Name:                   fmt.Sprintf("drain-svc-%d", i),
			Namespace:              "default",
			ServiceID:              svc.ID,
			ServiceName:            svc.Name,
			Status:                 types.InstanceStatusRunning,
			ContainerEverCreatedAt: &now,
			CreatedAt:              now,
			UpdatedAt:              now,
		}
		require.NoError(t, st.CreateInstance(ctx, inst))
		require.NoError(t, tr.Create(ctx, inst))
		insts = append(insts, inst)
	}
	return ctx, st, tr, c, lg, insts
}

// TestDeleteInstance_WithdrawsBeforeStop is the core Phase 0 ordering
// assertion: the endpoint set shrinks (and the store says Terminating)
// strictly before runner.Stop fires.
func TestDeleteInstance_WithdrawsBeforeStop(t *testing.T) {
	ctx, st, tr, c, lg, insts := setupDrainFixture(t, 1, 20*time.Millisecond)
	inst := insts[0]

	tr.StopFunc = func(sctx context.Context, si *types.Instance, _ time.Duration) error {
		stored, err := st.GetInstanceByID(sctx, "default", si.ID)
		require.NoError(t, err)
		assert.Equal(t, types.InstanceStatusTerminating, stored.Status,
			"at stop time the record must already be Terminating (withdrawn)")
		lg.add("stop")
		return nil
	}

	require.NoError(t, c.DeleteInstance(ctx, inst))

	iPub, iStop := lg.indexOf("publish:0"), lg.indexOf("stop")
	require.NotEqual(t, -1, iPub, "the withdrawal publish must happen: events=%v", lg.snapshot())
	require.NotEqual(t, -1, iStop, "runner.Stop must happen")
	assert.Less(t, iPub, iStop, "withdrawal must be published BEFORE the container is stopped: events=%v", lg.snapshot())

	stored, err := st.GetInstanceByID(ctx, "default", inst.ID)
	require.NoError(t, err)
	assert.Equal(t, types.InstanceStatusDeleted, stored.Status)
}

// TestDeleteInstance_SkipsDrainWhenNotServing: a non-Running instance was
// never in the endpoint set, so its teardown must not pay the drain window.
func TestDeleteInstance_SkipsDrainWhenNotServing(t *testing.T) {
	ctx, st, _, c, _, insts := setupDrainFixture(t, 1, 400*time.Millisecond)
	inst := insts[0]
	inst.Status = types.InstanceStatusStopped
	require.NoError(t, st.Update(ctx, types.ResourceTypeInstance, "default", inst.ID, inst))

	start := time.Now()
	require.NoError(t, c.DeleteInstance(ctx, inst))
	assert.Less(t, time.Since(start), 200*time.Millisecond,
		"teardown of a non-serving instance must not wait out the drain window")
}

// TestStopInstance_WithdrawsBeforeStop_AndRevertsOnFailure: stop uses the
// same withdraw-first ordering, and a runner failure rolls the Terminating
// flip back so the record keeps telling the truth.
func TestStopInstance_WithdrawsBeforeStop_AndRevertsOnFailure(t *testing.T) {
	ctx, st, tr, c, lg, insts := setupDrainFixture(t, 1, 20*time.Millisecond)
	inst := insts[0]

	tr.StopFunc = func(sctx context.Context, si *types.Instance, _ time.Duration) error {
		stored, err := st.GetInstanceByID(sctx, "default", si.ID)
		require.NoError(t, err)
		assert.Equal(t, types.InstanceStatusTerminating, stored.Status)
		lg.add("stop")
		return errors.New("boom")
	}

	err := c.StopInstance(ctx, inst)
	require.Error(t, err, "runner failure must surface")
	assert.Less(t, lg.indexOf("publish:0"), lg.indexOf("stop"), "withdraw before stop: %v", lg.snapshot())

	stored, gerr := st.GetInstanceByID(ctx, "default", inst.ID)
	require.NoError(t, gerr)
	assert.Equal(t, types.InstanceStatusRunning, stored.Status,
		"failed stop must revert the withdrawal flip — the container is still running")
}

// TestWithdrawServiceInstances_OneSharedDrain: a whole-service teardown
// takes ONE drain window for N instances, and the per-instance deletes
// that follow skip their own drains.
func TestWithdrawServiceInstances_OneSharedDrain(t *testing.T) {
	ctx, st, _, c, _, insts := setupDrainFixture(t, 3, 150*time.Millisecond)
	svc, err := st.GetService(ctx, "default", "drain-svc")
	require.NoError(t, err)

	start := time.Now()
	c.WithdrawServiceInstances(ctx, svc, insts)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 140*time.Millisecond, "the shared drain window must be taken")
	assert.Less(t, elapsed, 450*time.Millisecond, "one window for the batch, not one per instance")

	for _, inst := range insts {
		stored, err := st.GetInstanceByID(ctx, "default", inst.ID)
		require.NoError(t, err)
		assert.Equal(t, types.InstanceStatusTerminating, stored.Status)
	}

	// The subsequent per-instance teardowns see Terminating and skip their
	// own drain windows.
	start = time.Now()
	for _, inst := range insts {
		require.NoError(t, c.DeleteInstance(ctx, inst))
	}
	assert.Less(t, time.Since(start), 200*time.Millisecond,
		"already-withdrawn instances must not drain again")
}

// TestRestartInstance_WithdrawsBeforeStop: the liveness-restart path had
// the identical stop-before-withdraw defect (design §6.3); it now flips
// Terminating + publishes before stopping the container it replaces.
func TestRestartInstance_WithdrawsBeforeStop(t *testing.T) {
	ctx, st, tr, c, lg, insts := setupDrainFixture(t, 1, 20*time.Millisecond)
	inst := insts[0]

	tr.StopFunc = func(sctx context.Context, si *types.Instance, _ time.Duration) error {
		stored, err := st.GetInstanceByID(sctx, "default", si.ID)
		require.NoError(t, err)
		assert.Equal(t, types.InstanceStatusTerminating, stored.Status,
			"at stop time the record must already be withdrawn")
		lg.add("stop")
		return nil
	}

	require.NoError(t, c.RestartInstance(ctx, inst, InstanceRestartReasonManual))
	assert.Less(t, lg.indexOf("publish:0"), lg.indexOf("stop"),
		"restart must withdraw before stopping: %v", lg.snapshot())

	// The old record is tombstoned and a replacement exists.
	stored, err := st.GetInstanceByID(ctx, "default", inst.ID)
	require.NoError(t, err)
	assert.Equal(t, types.InstanceStatusFailed, stored.Status, "old record becomes the tombstone")
	assert.NotEmpty(t, tr.CreatedInstances, "a replacement instance must be created")
}
