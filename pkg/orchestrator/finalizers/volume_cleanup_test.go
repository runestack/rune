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
