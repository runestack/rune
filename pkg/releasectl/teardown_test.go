package releasectl

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tombstone marks the stored service as being deleted, the state Uninstall
// leaves behind while the reconciler cleans up.
func tombstone(t *testing.T, c *Controller, name string) *orchestrator.FakeOrchestrator {
	t.Helper()
	fake := c.orch.(*orchestrator.FakeOrchestrator)
	fake.TombstoneOnDelete(true)
	_, err := fake.DeleteService(context.Background(), &types.DeletionRequest{Namespace: "default", Name: name})
	require.NoError(t, err)
	return fake
}

// finishTeardownAfter removes the record after d, as the reconciler does once
// the last finalizer pops.
func finishTeardownAfter(fake *orchestrator.FakeOrchestrator, name string, d time.Duration) {
	go func() {
		time.Sleep(d)
		fake.TombstoneOnDelete(false)
		_, _ = fake.DeleteService(context.Background(), &types.DeletionRequest{Namespace: "default", Name: name})
	}()
}

func TestApply_WaitsOutATeardownThenCreates(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	require.NoError(t, c.orch.CreateService(ctx, stateService("OLD", "nginx:1", 1)))
	finishTeardownAfter(tombstone(t, c, "web"), "web", 300*time.Millisecond)

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{ref.Key(): stateService("", "nginx:2", 1)}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.Equal(t, "nginx:2", after.Image)
	assert.Nil(t, after.Metadata.DeletionTimestamp)
	assert.NotEqual(t, "OLD", after.ID, "a new service, not the torn-down one")
}

func TestApply_TeardownTimeoutNamesThePendingCleanup(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	require.NoError(t, c.orch.CreateService(ctx, stateService("OLD", "nginx:1", 1)))
	tombstone(t, c, "web")

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{ref.Key(): stateService("", "nginx:2", 1)}}}
	start := time.Now()
	err := a.Apply(ctx, release.PlannedChange{Ref: ref})
	require.Error(t, err)
	assert.GreaterOrEqual(t, time.Since(start), c.teardownWait, "it waited before giving up")
	assert.Contains(t, err.Error(), "was not gone after 1.5s")
	assert.Contains(t, err.Error(), "cleanup still pending: "+string(types.FinalizerTypeInstanceCleanup))
	assert.Contains(t, err.Error(), "rune delete service web -n default --abandon-finalizers")
}

func TestUninstallThenCast_Succeeds(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	ref := svcRef("default", "web")
	spec := release.ReleaseSpec{Name: "app", Namespace: "default", Resources: []release.DesiredResource{{Ref: ref}}}
	payloads := func() Payloads {
		return Payloads{Services: map[string]*types.Service{ref.Key(): {Name: "web", Namespace: "default", Image: "nginx:1"}}}
	}
	_, _, err := c.Cast(ctx, spec, payloads())
	require.NoError(t, err)

	fake := c.orch.(*orchestrator.FakeOrchestrator)
	fake.TombstoneOnDelete(true)
	require.NoError(t, c.Uninstall(ctx, "default", "app", UninstallOptions{}))
	finishTeardownAfter(fake, "web", 300*time.Millisecond)

	rel, _, err := c.Cast(ctx, spec, payloads())
	require.NoError(t, err, "the second cast waits for the first teardown instead of refusing")
	assert.Equal(t, types.ReleaseStatusDeployed, rel.Status)
}

func TestApply_TeardownWaitEndsWithTheCaller(t *testing.T) {
	c, _ := newTestController(t)
	require.NoError(t, c.orch.CreateService(context.Background(), stateService("OLD", "nginx:1", 1)))
	tombstone(t, c, "web")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{ref.Key(): stateService("", "nginx:2", 1)}}}
	err := a.Apply(ctx, release.PlannedChange{Ref: ref})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "cleanup still pending", "the caller's deadline keeps the diagnosis")
}

// A delete that starts after the cast began is an operator's decision; the
// cast refuses rather than waiting it out and recreating the service.
func TestApply_TeardownStartedDuringTheCastIsRefused(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	require.NoError(t, c.orch.CreateService(ctx, stateService("OLD", "nginx:1", 1)))
	ref := svcRef("default", "web")
	a := &applier{c: c, started: time.Now().Add(-time.Second),
		p: Payloads{Services: map[string]*types.Service{ref.Key(): stateService("", "nginx:2", 1)}}}
	tombstone(t, c, "web")

	start := time.Now()
	err := a.Apply(ctx, release.PlannedChange{Ref: ref})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "being deleted")
	assert.Less(t, time.Since(start), c.teardownWait, "refused without waiting")
}

func TestApply_DoesNotTakeOverAServiceRecreatedDuringTheWait(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	require.NoError(t, c.orch.CreateService(ctx, stateService("OLD", "nginx:1", 1)))
	fake := tombstone(t, c, "web")
	go func() {
		time.Sleep(100 * time.Millisecond)
		fake.TombstoneOnDelete(false)
		_, _ = fake.DeleteService(context.Background(), &types.DeletionRequest{Namespace: "default", Name: "web"})
		_ = fake.CreateService(context.Background(), stateService("THEIRS", "theirs:1", 1))
	}()

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{ref.Key(): stateService("", "nginx:2", 1)}}}
	err := a.Apply(ctx, release.PlannedChange{Ref: ref})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "created by someone else")

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.Equal(t, "theirs:1", after.Image)
}

// createRaces loses every create to someone else's service of the same name.
type createRaces struct{ *orchestrator.FakeOrchestrator }

func (createRaces) UpdateServiceFunc(context.Context, string, string, func(*types.Service) error) error {
	return orchestrator.ErrServiceNotFound
}

func (createRaces) CreateService(_ context.Context, s *types.Service) error {
	return fmt.Errorf("resource service/%s/%s already exists", s.Namespace, s.Name)
}

func TestRevert_LeavesAServiceThisCastFailedToCreate(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	fake := c.orch.(*orchestrator.FakeOrchestrator)
	require.NoError(t, fake.CreateService(ctx, stateService("THEIRS", "theirs:1", 1)))
	c.orch = createRaces{fake}

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{ref.Key(): stateService("", "nginx:2", 1)}}}
	require.Error(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))
	require.NoError(t, a.Revert(ctx, ref))

	after, err := fake.GetService(ctx, "default", "web")
	require.NoError(t, err, "a revert must not delete a service this cast never created")
	assert.Equal(t, "theirs:1", after.Image)
}
