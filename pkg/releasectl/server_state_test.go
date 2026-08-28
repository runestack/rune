package releasectl

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeLegacyService stores a service whose metadata counters are all zero,
// which is what older records on disk look like.
func storeLegacyService(t *testing.T, ctx context.Context, c *Controller, svc *types.Service) {
	t.Helper()
	require.NoError(t, c.orch.CreateService(ctx, svc))
	zeroed := *svc
	zeroed.Metadata = &types.ServiceMetadata{}
	require.NoError(t, c.orch.UpdateService(ctx, &zeroed))
}

func stateService(id, image string, scale int) *types.Service {
	return &types.Service{
		ID: id, Name: "web", Namespace: "default", Image: image, Scale: scale,
		Status:   types.ServiceStatusPending,
		Metadata: &types.ServiceMetadata{},
	}
}

func TestApply_CarriesServerStateOntoTheRenderedService(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	born := time.Now().Add(-30 * 24 * time.Hour)
	stored := stateService("STABLE-ID", "nginx:1", 3)
	stored.Metadata = &types.ServiceMetadata{
		Generation: 12, TemplateGeneration: 9, ObservedGeneration: 12,
		LastNonZeroScale: 3, CreatedAt: born,
	}
	stored.IngressCert = &types.IngressCertStatus{Host: "api.example.com", State: types.IngressCertIssued}
	stored.Metadata.OwnedBy = &types.OwnedBy{Release: "app", Revision: 1}
	require.NoError(t, c.orch.CreateService(ctx, stored))

	ref := svcRef("default", "web")
	// Asserted by the payload, which is caller-supplied JSON: a wedged roll's
	// high-water marks would make the next roll need more ready replicas than
	// it has before anything counts as progress.
	rendered := stateService("FRESHLY-MINTED", "nginx:2", 3)
	rendered.Status = types.ServiceStatusRunning
	rendered.StatusReason = "converged"
	rendered.StatusMessage = "3/3 ready"
	rendered.Update = &types.UpdateStatus{TemplateGeneration: 9, Desired: 3, UpdatedReady: 2}
	a := &applier{c: c, stamp: types.OwnedBy{Release: "app", Revision: 2},
		p: Payloads{Services: map[string]*types.Service{ref.Key(): rendered}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)

	assert.Equal(t, "STABLE-ID", after.ID)
	assert.EqualValues(t, 13, after.Metadata.Generation)
	assert.EqualValues(t, 13, after.Metadata.TemplateGeneration)
	assert.EqualValues(t, 12, after.Metadata.ObservedGeneration)
	assert.EqualValues(t, 3, after.Metadata.LastNonZeroScale)
	assert.Equal(t, born.Unix(), after.Metadata.CreatedAt.Unix())
	assert.True(t, after.Metadata.UpdatedAt.After(born), "an apply stamps the update time")
	require.NotNil(t, after.IngressCert)
	assert.Equal(t, types.IngressCertIssued, after.IngressCert.State)
	assert.Equal(t, types.ServiceStatusPending, after.Status)
	assert.Empty(t, after.StatusMessage)
	require.NotNil(t, after.Metadata.OwnedBy)
	assert.EqualValues(t, 2, after.Metadata.OwnedBy.Revision,
		"the casting release's stamp wins; a stored one would freeze ownership at its revision")
	// Reset, not carried: Verify polls Status, so a payload claiming Running
	// would report the rollout complete before a container had been replaced.
	assert.Equal(t, types.ServiceStatusPending, after.Status)
	assert.Empty(t, after.StatusReason)
	assert.Empty(t, after.StatusMessage)
	assert.Nil(t, after.Update, "stall high-water marks must not seed the next roll")
	assert.Equal(t, "nginx:2", after.Image)
}

func TestApply_ScaleChangeDoesNotMoveTheTemplate(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	stored := stateService("STABLE-ID", "nginx:1", 1)
	stored.Metadata = &types.ServiceMetadata{Generation: 12, TemplateGeneration: 9}
	require.NoError(t, c.orch.CreateService(ctx, stored))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:1", 5),
	}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.EqualValues(t, 13, after.Metadata.Generation)
	assert.EqualValues(t, 9, after.Metadata.TemplateGeneration, "scale must not replace instances")
}

// An unchanged castfile must not move the counters or the ID.
func TestApply_UnchangedSpecChangesNothing(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	stored := stateService("STABLE-ID", "nginx:1", 2)
	stored.Metadata = &types.ServiceMetadata{Generation: 12, TemplateGeneration: 9}
	require.NoError(t, c.orch.CreateService(ctx, stored))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:1", 2),
	}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.EqualValues(t, 12, after.Metadata.Generation)
	assert.EqualValues(t, 9, after.Metadata.TemplateGeneration)
	assert.Equal(t, "STABLE-ID", after.ID)
}

// Adopting a template generation for a service whose counters are zeroed
// would mark its instances — all stamped 0 — outdated, rolling the whole fleet
// for a scale edit.
func TestApply_ZeroedCountersDoNotRollTheFleetOnAScaleEdit(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	storeLegacyService(t, ctx, c, stateService("STABLE-ID", "nginx:1", 1))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:1", 5),
	}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.EqualValues(t, 1, after.Metadata.Generation, "scale changed, so the desired state did")
	assert.EqualValues(t, 0, after.Metadata.TemplateGeneration,
		"nothing about the container changed; instances stamped 0 must stay compatible")
	assert.EqualValues(t, 5, after.Metadata.LastNonZeroScale,
		"a legacy zero here leaves `rune restart` returning to scale 1")
}

// The counter still starts on the first real template change, which is when a
// roll is expected anyway.
func TestApply_ZeroedCountersStartOnATemplateChange(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	storeLegacyService(t, ctx, c, stateService("STABLE-ID", "nginx:1", 1))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:2", 1),
	}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.EqualValues(t, 1, after.Metadata.TemplateGeneration)
}

func TestApply_KeepsTheAllocatedVIP(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	stored := stateService("STABLE-ID", "nginx:1", 1)
	stored.Discovery = &types.ServiceDiscovery{VIP: "10.42.0.7", Mode: "load-balanced"}
	require.NoError(t, c.orch.CreateService(ctx, stored))

	ref := svcRef("default", "web")
	rendered := stateService("FRESH", "nginx:1", 1)
	rendered.Discovery = &types.ServiceDiscovery{Mode: "headless"}
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{ref.Key(): rendered}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	require.NotNil(t, after.Discovery)
	assert.Equal(t, "10.42.0.7", after.Discovery.VIP, "control-plane allocation")
	assert.Equal(t, "headless", after.Discovery.Mode, "operator intent")
}

// A green cast that leaves no service: the reconciler deletes the record the
// apply just wrote.
func TestApply_RefusesAServiceBeingDeleted(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	deleting := time.Now().Add(-time.Minute)
	stored := stateService("STABLE-ID", "nginx:1", 1)
	// Timestamp only: the finalizers may already have drained, and the record
	// is still being torn down.
	stored.Metadata = &types.ServiceMetadata{DeletionTimestamp: &deleting}
	require.NoError(t, c.orch.CreateService(ctx, stored))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:2", 1),
	}}}
	err := a.Apply(ctx, release.PlannedChange{Ref: ref})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "being deleted")

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.Equal(t, "nginx:1", after.Image, "the spec must not have been written")
}

func TestCarryServerState_NilSafe(t *testing.T) {
	rendered := stateService("NEW", "nginx:1", 1)
	carryServerState(nil, rendered)
	assert.Equal(t, "NEW", rendered.ID, "a nil stored service must change nothing")

	carryServerState(&types.Service{}, rendered)
	assert.Equal(t, "NEW", rendered.ID, "an empty stored ID must not blank the rendered one")
	require.NotNil(t, rendered.Metadata, "a stored service without metadata must not leave it nil")
	assert.EqualValues(t, 1, rendered.Metadata.Generation, "an empty stored spec differs, so the counter moves")
	assert.EqualValues(t, 1, rendered.Metadata.LastNonZeroScale)
}

func TestRevert_MovesTheTemplateForwardNotBack(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	stored := stateService("STABLE-ID", "nginx:1", 1)
	stored.Metadata = &types.ServiceMetadata{Generation: 12, TemplateGeneration: 9}
	require.NoError(t, c.orch.CreateService(ctx, stored))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:2", 1),
	}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	rolled, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	require.EqualValues(t, 13, rolled.Metadata.TemplateGeneration)

	require.NoError(t, a.Revert(ctx, ref))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.Equal(t, "nginx:1", after.Image, "the spec rolls back")
	assert.Greater(t, after.Metadata.TemplateGeneration, int64(13),
		"instances the failed cast created must read as outdated")
	assert.Equal(t, "STABLE-ID", after.ID)
}

// A rollback lifts a teardown this release started, or the prune it is undoing
// stands and the restored service deletes itself.
func TestRevert_LiftsATeardownThisReleasePruned(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	c.orch.(*orchestrator.FakeOrchestrator).TombstoneOnDelete(true)

	require.NoError(t, c.orch.CreateService(ctx, stateService("STABLE-ID", "nginx:1", 1)))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:1", 1),
	}}}
	require.NoError(t, a.Prune(ctx, ref))

	tombstoned, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	require.NotNil(t, tombstoned.Metadata.DeletionTimestamp, "prune tombstones rather than deleting")

	require.NoError(t, a.Revert(ctx, ref))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.Nil(t, after.Metadata.DeletionTimestamp)
	assert.Empty(t, after.Metadata.Finalizers)
}

// ...but not one somebody else started. An operator deleting a service during
// the release's verify window is their decision; a rollback that undoes it
// lets releases:create cancel a services:delete.
func TestRevert_LeavesATeardownItDidNotStart(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	require.NoError(t, c.orch.CreateService(ctx, stateService("STABLE-ID", "nginx:1", 1)))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:2", 1),
	}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	deleted, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	now := time.Now()
	deleted.Metadata.DeletionTimestamp = &now
	deleted.Metadata.Finalizers = []types.FinalizerType{types.FinalizerTypeInstanceCleanup}
	require.NoError(t, c.orch.UpdateService(ctx, deleted))

	require.NoError(t, a.Revert(ctx, ref))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	require.NotNil(t, after.Metadata.DeletionTimestamp, "the delete must still be in flight")
	assert.Equal(t, []types.FinalizerType{types.FinalizerTypeInstanceCleanup}, after.Metadata.Finalizers)
}

// If the record vanished between apply and revert, there is nothing live to
// carry — but the instances the failed cast created are still stamped with the
// generation it reached, so recreating at the pre-image's counter leaves them
// permanently outranking the restored spec.
func TestRevert_RecreatesAboveTheGenerationTheFailedCastReached(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	stored := stateService("STABLE-ID", "nginx:1", 1)
	stored.Metadata = &types.ServiceMetadata{Generation: 9, TemplateGeneration: 9}
	require.NoError(t, c.orch.CreateService(ctx, stored))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:2", 1),
	}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	rolled, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	reached := rolled.Metadata.TemplateGeneration
	require.NoError(t, a.Prune(ctx, ref))

	require.NoError(t, a.Revert(ctx, ref))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.Equal(t, "nginx:1", after.Image)
	assert.Greater(t, after.Metadata.TemplateGeneration, reached,
		"instances the failed cast created are stamped at the generation it reached")
}

// A merge is right for an apply, where the incoming payload is the operator's
// intent, and wrong for a revert, where an omitted field means "put it back to
// what it was" — merging keeps whatever the failed cast set.
func TestRevert_RestoresDiscoveryFieldsTheFailedCastSet(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	stored := stateService("STABLE-ID", "nginx:1", 1)
	stored.Discovery = &types.ServiceDiscovery{VIP: "10.42.0.7"}
	require.NoError(t, c.orch.CreateService(ctx, stored))

	ref := svcRef("default", "web")
	rendered := stateService("FRESH", "nginx:2", 1)
	rendered.Discovery = &types.ServiceDiscovery{Mode: "headless", LocalityPreference: "zone"}
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{ref.Key(): rendered}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	require.NoError(t, a.Revert(ctx, ref))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	require.NotNil(t, after.Discovery)
	assert.Equal(t, "10.42.0.7", after.Discovery.VIP, "the allocation survives a rollback")
	assert.Empty(t, after.Discovery.Mode, "the failed cast's mode must not survive it")
	assert.Empty(t, after.Discovery.LocalityPreference)
}

// Verify is the release's readiness gate: it is what makes --atomic able to
// observe a bad rollout, and what holds the destructive prune until the new
// instances exist.
func TestVerify_ReportsAServiceThatEnteredFailed(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	svc := stateService("STABLE-ID", "nginx:1", 1)
	svc.Status = types.ServiceStatusFailed
	require.NoError(t, c.orch.CreateService(ctx, svc))

	a := &applier{c: c}
	err := a.Verify(ctx, []types.OwnerRef{svcRef("default", "web")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Failed")
}

func TestVerify_PassesOnARunningService(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	svc := stateService("STABLE-ID", "nginx:1", 1)
	svc.Status = types.ServiceStatusRunning
	require.NoError(t, c.orch.CreateService(ctx, svc))

	a := &applier{c: c}
	require.NoError(t, a.Verify(ctx, []types.OwnerRef{svcRef("default", "web")}))
}

// The pre-image predates the allocation when a service gets its VIP between
// the apply and the rollback, so restoring the pre-image's discovery verbatim
// would hand back a service with no address.
func TestRevert_KeepsAVIPAllocatedAfterThePreImage(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	stored := stateService("STABLE-ID", "nginx:1", 1)
	stored.Discovery = &types.ServiceDiscovery{Mode: "load-balanced"}
	require.NoError(t, c.orch.CreateService(ctx, stored))

	ref := svcRef("default", "web")
	drifted := stateService("FRESH", "nginx:2", 1)
	drifted.Discovery = &types.ServiceDiscovery{Mode: "headless"}
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{ref.Key(): drifted}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	allocated, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	allocated.Discovery.VIP = "10.42.0.9"
	require.NoError(t, c.orch.UpdateService(ctx, allocated))

	require.NoError(t, a.Revert(ctx, ref))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	require.NotNil(t, after.Discovery)
	assert.Equal(t, "10.42.0.9", after.Discovery.VIP)
	assert.Equal(t, "load-balanced", after.Discovery.Mode,
		"the pre-image's mode, not the one the failed cast set")
}

// A rendered payload is caller-supplied JSON of the internal type, so it can
// carry server-owned metadata. A generation near the ceiling overflows on the
// next cast, after which every instance outranks its service and no cast can
// replace them.
func TestApply_DiscardsServerStateOnAPayloadThatCreatesAService(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	ref := svcRef("default", "web")
	hostile := stateService("CHOSEN-ID", "nginx:1", 1)
	hostile.Metadata = &types.ServiceMetadata{
		Generation:         math.MaxInt64,
		TemplateGeneration: math.MaxInt64,
		ObservedGeneration: math.MaxInt64,
	}
	hostile.Status = types.ServiceStatusRunning
	hostile.Update = &types.UpdateStatus{Desired: 99, UpdatedReady: 99}
	hostile.IngressCert = &types.IngressCertStatus{Host: "attacker.example.com", State: types.IngressCertIssued}
	hostile.Discovery = &types.ServiceDiscovery{Mode: "load-balanced", VIP: "10.42.0.7"}
	hostile.Instances = []types.Instance{{ID: "ghost", Name: "web-0", Status: types.InstanceStatusRunning}}
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{ref.Key(): hostile}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.EqualValues(t, 1, after.Metadata.Generation)
	assert.EqualValues(t, 1, after.Metadata.TemplateGeneration)
	assert.EqualValues(t, 0, after.Metadata.ObservedGeneration)
	assert.NotEqual(t, types.ServiceStatusRunning, after.Status)
	assert.Nil(t, after.Update)
	assert.Nil(t, after.IngressCert)
	assert.NotEqual(t, "CHOSEN-ID", after.ID,
		"a chosen ID adopts another service's instances and shares its VIP allocation")
	require.NotNil(t, after.Discovery)
	assert.Empty(t, after.Discovery.VIP,
		"the dataplane programs the record's VIP as a /32 on every node")
	assert.Equal(t, "load-balanced", after.Discovery.Mode, "operator intent survives")
	assert.Empty(t, after.Instances, "the instance list is derived, and renders in `rune get`")
	// The payload is the caller's; sanitizing must not write back through it.
	assert.Equal(t, "CHOSEN-ID", hostile.ID)
}

// A rollback must outrank only what the failed cast actually created. Raising
// the template counter when the cast never moved it rolls every survivor, for
// a spec they are already running.
func TestRevert_RecreateDoesNotRollSurvivorsAScaleCastNeverTouched(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	stored := stateService("STABLE-ID", "nginx:1", 2)
	stored.Metadata = &types.ServiceMetadata{Generation: 12, TemplateGeneration: 9}
	require.NoError(t, c.orch.CreateService(ctx, stored))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:1", 6), // scale only
	}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))
	require.NoError(t, a.Prune(ctx, ref))

	require.NoError(t, a.Revert(ctx, ref))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.Greater(t, after.Metadata.Generation, int64(13), "the record must not go backwards")
	assert.EqualValues(t, 9, after.Metadata.TemplateGeneration,
		"the cast never changed the template, so nothing running is stale")
}

// A rollback restores what this release removed. A service someone else deleted
// while the release was in flight is gone by their decision, and recreating it
// would let releases:create undo a services:delete just by outwaiting the
// finalizers.
func TestRevert_DoesNotResurrectAServiceItDidNotPrune(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	require.NoError(t, c.orch.CreateService(ctx, stateService("STABLE-ID", "nginx:1", 1)))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:2", 1),
	}}}
	require.NoError(t, a.Apply(ctx, release.PlannedChange{Ref: ref}))

	_, err := c.orch.DeleteService(ctx, &types.DeletionRequest{Namespace: "default", Name: "web"})
	require.NoError(t, err)

	require.NoError(t, a.Revert(ctx, ref))

	_, err = c.orch.GetService(ctx, "default", "web")
	require.Error(t, err, "the operator's delete must stand")
}

// A prune that finds a teardown already under way has not started it, so a
// later rollback must not lift it.
func TestRevert_LeavesATeardownAPruneMerelyFound(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	c.orch.(*orchestrator.FakeOrchestrator).TombstoneOnDelete(true)

	stored := stateService("STABLE-ID", "nginx:1", 1)
	require.NoError(t, c.orch.CreateService(ctx, stored))
	_, err := c.orch.DeleteService(ctx, &types.DeletionRequest{Namespace: "default", Name: "web"})
	require.NoError(t, err)

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:1", 1),
	}}}
	require.NoError(t, a.Prune(ctx, ref))
	require.NoError(t, a.Revert(ctx, ref))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.NotNil(t, after.Metadata.DeletionTimestamp, "the teardown was not this release's to undo")
}

// A record whose counter reached the ceiling can never advance again, so no
// later spec change would reach a container. Refusing makes that visible; the
// old behaviour reported success and changed nothing.
func TestApply_RefusesAServiceWhoseGenerationIsExhausted(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	stored := stateService("STABLE-ID", "nginx:1", 1)
	stored.Metadata = &types.ServiceMetadata{Generation: math.MaxInt64, TemplateGeneration: math.MaxInt64}
	require.NoError(t, c.orch.CreateService(ctx, stored))

	ref := svcRef("default", "web")
	a := &applier{c: c, p: Payloads{Services: map[string]*types.Service{
		ref.Key(): stateService("FRESH", "nginx:2", 1),
	}}}
	err := a.Apply(ctx, release.PlannedChange{Ref: ref})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exhausted generation")

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.Equal(t, "nginx:1", after.Image, "nothing may have been written")
}

// A rollback clamps instead of refusing: the record is already wedged, and a
// revert that fails leaves worse state than one that restores the spec.
func TestRevert_RecreateAtTheCeilingClampsRatherThanWrapping(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	ref := svcRef("default", "web")
	wedged := stateService("STABLE-ID", "nginx:1", 1)
	wedged.Metadata = &types.ServiceMetadata{
		Generation: math.MaxInt64, TemplateGeneration: math.MaxInt64,
	}
	a := &applier{c: c}
	a.capture(ref, preImage{Existed: true, Service: wedged, Pruned: true})

	require.NoError(t, a.Revert(ctx, ref))

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.EqualValues(t, int64(math.MaxInt64), after.Metadata.Generation)
	assert.Positive(t, after.Metadata.TemplateGeneration)
}

// The live record is the authority on the VIP, including when it has none:
// re-asserting a released address can point it at whoever holds it now.
func TestRestoreDiscovery_TakesTheVIPFromTheLiveRecord(t *testing.T) {
	preImage := &types.ServiceDiscovery{Mode: "load-balanced", VIP: "10.42.0.7"}
	assert.Equal(t, "10.42.0.9", restoreDiscovery(preImage, "10.42.0.9").VIP)
	assert.Empty(t, restoreDiscovery(preImage, "").VIP, "the allocator has released it")
	assert.Equal(t, "load-balanced", restoreDiscovery(preImage, "").Mode)
	assert.Equal(t, &types.ServiceDiscovery{VIP: "10.42.0.9"}, restoreDiscovery(nil, "10.42.0.9"))
	assert.Nil(t, restoreDiscovery(nil, ""))
}

// Verify is what makes --atomic able to see a bad rollout: it must not return
// while the service is still converging.
func TestVerify_WaitsWhileAServiceIsStillDeploying(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()

	svc := stateService("STABLE-ID", "nginx:1", 1)
	svc.Status = types.ServiceStatusDeploying
	require.NoError(t, c.orch.CreateService(ctx, svc))

	a := &applier{c: c}
	err := a.Verify(ctx, []types.OwnerRef{svcRef("default", "web")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timed out", "it waited rather than reporting success")
}
