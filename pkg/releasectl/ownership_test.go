package releasectl

import (
	"context"
	"errors"
	"testing"

	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func otherOwned(image string) *types.Service {
	s := stateService("THEIRS", image, 1)
	s.Metadata.OwnedBy = &types.OwnedBy{Release: "other", Namespace: "default"}
	return s
}

// The plan saw our service, but by prune time someone else's holds the name.
func TestPrune_LeavesAResourceAnotherOwnerNowHolds(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	require.NoError(t, c.orch.CreateService(ctx, otherOwned("theirs:1")))

	ref := svcRef("default", "web")
	a := &applier{c: c, stamp: appStamp}
	require.NoError(t, a.Prune(ctx, ref))
	require.NoError(t, a.Revert(ctx, ref), "nothing was touched, so nothing to revert")

	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err)
	assert.Equal(t, "theirs:1", after.Image)
	assert.Nil(t, after.Metadata.DeletionTimestamp)
}

func TestUninstall_LeavesWhatAnotherReleaseNowOwns(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	ref := svcRef("default", "web")
	spec := release.ReleaseSpec{Name: "app", Namespace: "default", Resources: []release.DesiredResource{{Ref: ref}}}
	_, _, err := c.Cast(ctx, spec, Payloads{Services: map[string]*types.Service{
		ref.Key(): {Name: "web", Namespace: "default", Image: "nginx:1"},
	}})
	require.NoError(t, err)

	// Deleted out of band and recreated by another release.
	fake := c.orch.(*orchestrator.FakeOrchestrator)
	_, err = fake.DeleteService(ctx, &types.DeletionRequest{Namespace: "default", Name: "web"})
	require.NoError(t, err)
	require.NoError(t, fake.CreateService(ctx, otherOwned("theirs:1")))

	require.NoError(t, c.Uninstall(ctx, "default", "app", UninstallOptions{}))
	after, err := c.orch.GetService(ctx, "default", "web")
	require.NoError(t, err, "uninstall must not delete another release's service")
	assert.Equal(t, "theirs:1", after.Image)
}

// failingGet makes every service lookup fail with something other than
// not-found.
type failingGet struct{ *orchestrator.FakeOrchestrator }

func (failingGet) GetService(context.Context, string, string) (*types.Service, error) {
	return nil, errors.New("store unavailable")
}

func TestPlan_AFailedLookupIsNotAnAbsentResource(t *testing.T) {
	c, _ := newTestController(t)
	c.orch = failingGet{c.orch.(*orchestrator.FakeOrchestrator)}
	ref := svcRef("default", "web")
	_, err := c.Plan(context.Background(), release.ReleaseSpec{
		Name: "app", Namespace: "default", Resources: []release.DesiredResource{{Ref: ref}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store unavailable")
}

// The API and CLI write secrets, configmaps and volumes without an owner.
// Those writes must not orphan the resource from the release that owns it.
func TestCast_StillOwnsWhatAnUnstampedWriteTouched(t *testing.T) {
	c, _ := newTestController(t)
	ctx := context.Background()
	sec := secretRef("default", "creds")
	cm := types.OwnerRef{ResourceType: types.ResourceTypeConfigmap, Namespace: "default", Name: "cfg"}
	vol := types.OwnerRef{ResourceType: types.ResourceTypeVolume, Namespace: "default", Name: "data"}
	spec := release.ReleaseSpec{Name: "app", Namespace: "default",
		Resources: []release.DesiredResource{{Ref: sec}, {Ref: cm}, {Ref: vol}}}
	payloads := func() Payloads {
		return Payloads{
			Secrets:    map[string]*types.Secret{sec.Key(): {Name: "creds", Namespace: "default", Type: "static", Data: map[string]string{"k": "v1"}}},
			Configmaps: map[string]*types.Configmap{cm.Key(): {Name: "cfg", Namespace: "default"}},
			Volumes:    map[string]*types.Volume{vol.Key(): {Name: "data", Namespace: "default", StorageClassName: "local", Size: "1Gi"}},
		}
	}
	_, _, err := c.Cast(ctx, spec, payloads())
	require.NoError(t, err)

	// What `rune secret patch`, `rune configmap update` and UpdateVolume write.
	require.NoError(t, c.secrets.Update(ctx, "default", "creds",
		&types.Secret{Name: "creds", Namespace: "default", Type: "static", Data: map[string]string{"k": "rotated"}}))
	require.NoError(t, c.configs.Update(ctx, "default", "cfg",
		&types.Configmap{Name: "cfg", Namespace: "default"}))
	stored, err := c.volumes.Get(ctx, "default", "data")
	require.NoError(t, err)
	unstamped := *stored
	unstamped.OwnedBy = nil
	require.NoError(t, c.volumes.Update(ctx, &unstamped))

	plan, err := c.Plan(ctx, spec)
	require.NoError(t, err)
	require.True(t, plan.Applyable(), "the release still owns all three: %+v", plan.Changes)
	for _, ch := range plan.Changes {
		assert.Equal(t, release.ActionUpdate, ch.Action, ch.Ref.Key())
	}
}
