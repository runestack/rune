package finalizers

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mkVolume(t *testing.T, ts *store.TestStore, ns, name, owner string) {
	t.Helper()
	v := &types.Volume{
		ID:           name,
		Name:         name,
		Namespace:    ns,
		OwnerService: owner,
		Status:       types.VolumeStatusAvailable,
		Handle:       "/srv/" + name,
		CreatedAt:    time.Now().UTC(),
	}
	require.NoError(t, ts.Create(context.Background(), types.ResourceTypeVolume, ns, name, v))
}

func TestVolumeCleanupFinalizer_DeletesOwnedVolumes(t *testing.T) {
	ts := store.NewTestStore()
	mkVolume(t, ts, "default", "data-api-0", "default/api")
	mkVolume(t, ts, "default", "data-api-1", "default/api")
	mkVolume(t, ts, "default", "shared", "")                    // operator-owned, must be left alone
	mkVolume(t, ts, "default", "data-other-0", "default/other") // owned by a different service

	f := NewVolumeCleanupFinalizer(ts, log.NewLogger())
	svc := &types.Service{Name: "api", Namespace: "default"}
	require.NoError(t, f.Execute(context.Background(), svc))

	var remaining []types.Volume
	require.NoError(t, ts.List(context.Background(), types.ResourceTypeVolume, "default", &remaining))
	names := make([]string, 0, len(remaining))
	for _, v := range remaining {
		names = append(names, v.Name)
	}
	assert.ElementsMatch(t, []string{"shared", "data-other-0"}, names)
}

func TestVolumeCleanupFinalizer_NoOwnedVolumesIsNoop(t *testing.T) {
	ts := store.NewTestStore()
	mkVolume(t, ts, "default", "shared", "")

	f := NewVolumeCleanupFinalizer(ts, log.NewLogger())
	svc := &types.Service{Name: "api", Namespace: "default"}
	require.NoError(t, f.Execute(context.Background(), svc))

	var remaining []types.Volume
	require.NoError(t, ts.List(context.Background(), types.ResourceTypeVolume, "default", &remaining))
	assert.Len(t, remaining, 1)
}

func TestVolumeCleanupFinalizer_ValidateRejectsNil(t *testing.T) {
	f := NewVolumeCleanupFinalizer(store.NewTestStore(), log.NewLogger())
	require.Error(t, f.Validate(nil))
	require.NoError(t, f.Validate(&types.Service{Name: "api", Namespace: "default"}))
}

// On `rune delete <service>`, volumes bound to that service's instances
// must have their BoundNode + BoundClaim cleared even when they are
// operator-owned (no OwnerService). Clearing the bind state is what
// triggers the agent's volumes Subsystem to Unmount + Detach the
// underlying cloud volume — without it, do-volume volumes stay
// attached to the droplet after `rune delete --force`, and an operator
// who later wants to delete them via DO API hits a 409.
func TestVolumeCleanupFinalizer_UnbindsClaimVolumes(t *testing.T) {
	ts := store.NewTestStore()
	// Operator-owned volume (no OwnerService) bound to an instance of
	// the service we're about to delete. The row must SURVIVE the
	// finalizer (operator-managed lifecycle) but the bind state must
	// be cleared so the agent tears the mount down.
	v := &types.Volume{
		ID:         "shared",
		Name:       "shared",
		Namespace:  "default",
		Status:     types.VolumeStatusBound,
		Handle:     "do-uuid-shared",
		BoundNode:  "node-a",
		BoundClaim: "default/api-0",
		CreatedAt:  time.Now().UTC(),
	}
	require.NoError(t, ts.Create(context.Background(), types.ResourceTypeVolume, "default", "shared", v))

	// Seed the instance row that's about to be cleaned up. Pretend
	// InstanceCleanupFinalizer has just marked it Deleted (the order
	// the executor invokes us in).
	ins := &types.Instance{
		ID:          "api-0-id",
		Name:        "api-0",
		Namespace:   "default",
		ServiceName: "api",
		Status:      types.InstanceStatusDeleted,
	}
	require.NoError(t, ts.Create(context.Background(), types.ResourceTypeInstance, "default", "api-0-id", ins))

	f := NewVolumeCleanupFinalizer(ts, log.NewLogger())
	svc := &types.Service{Name: "api", Namespace: "default"}
	require.NoError(t, f.Execute(context.Background(), svc))

	var after types.Volume
	require.NoError(t, ts.Get(context.Background(), types.ResourceTypeVolume, "default", "shared", &after))
	assert.Equal(t, "", after.BoundNode, "BoundNode must be cleared so the agent tears down the mount")
	assert.Equal(t, "", after.BoundClaim, "BoundClaim must be cleared")
	assert.Equal(t, types.VolumeStatusAvailable, after.Status, "status flips back to Available so the row is reusable")
	assert.Equal(t, "do-uuid-shared", after.Handle, "operator-owned Volume row survives — only the bind is released")
}
