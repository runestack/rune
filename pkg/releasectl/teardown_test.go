package releasectl

import (
	"context"
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
	assert.Contains(t, err.Error(), "cast again once teardown finishes")
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
}
