package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolveVolumeMount unit tests (RUNE-070). Exercises only the volume
// branch of resolveMounts; the helper is package-private so we cast
// the InstanceController back to its concrete type to invoke it.
func newInstanceControllerForVolumeTests(t *testing.T) (*store.TestStore, *instanceController) {
	t.Helper()
	ts := store.NewTestStore()
	ic := NewInstanceController(ts, manager.NewTestRunnerManager(nil), log.NewLogger())
	concrete, ok := ic.(*instanceController)
	require.True(t, ok, "InstanceController must be the in-package concrete type")
	return ts, concrete
}

func putVolume(t *testing.T, ts *store.TestStore, ns, name, handle string, status types.VolumeStatus) {
	t.Helper()
	v := &types.Volume{
		ID:        "vol-" + name,
		Name:      name,
		Namespace: ns,
		Status:    status,
		Handle:    handle,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, ts.Create(context.Background(), types.ResourceTypeVolume, ns, name, v))
}

func TestResolveVolumeMount_ClaimSuccess(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "default", "data", "/var/lib/rune/volumes/default/data", types.VolumeStatusAvailable)

	svc := &types.Service{Name: "api", Namespace: "default"}
	got, err := ic.resolveVolumeMount(context.Background(), svc, types.VolumeMount{
		Name:      "data",
		MountPath: "/data",
		Claim:     &types.VolumeClaim{Name: "data"},
	})
	require.NoError(t, err)
	assert.Equal(t, "data", got.Name)
	assert.Equal(t, "/data", got.MountPath)
	assert.Equal(t, "/var/lib/rune/volumes/default/data", got.Source)
	assert.Equal(t, "data", got.VolumeName)
	assert.Equal(t, "default", got.VolumeNamespace)
}

func TestResolveVolumeMount_ClaimReadOnlyAndSubPath(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "default", "shared", "/srv/shared", types.VolumeStatusAvailable)

	svc := &types.Service{Name: "api", Namespace: "default"}
	got, err := ic.resolveVolumeMount(context.Background(), svc, types.VolumeMount{
		Name:      "shared",
		MountPath: "/etc/shared",
		ReadOnly:  true,
		SubPath:   "config",
		Claim:     &types.VolumeClaim{Name: "shared"},
	})
	require.NoError(t, err)
	assert.True(t, got.ReadOnly)
	assert.Equal(t, "config", got.SubPath)
	assert.Equal(t, "/srv/shared", got.Source, "subpath join happens in the runner, not the resolver")
}

func TestResolveVolumeMount_CrossNamespaceClaim(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "shared-ns", "blob", "/srv/blob", types.VolumeStatusAvailable)

	svc := &types.Service{Name: "api", Namespace: "default"}
	got, err := ic.resolveVolumeMount(context.Background(), svc, types.VolumeMount{
		Name:      "blob",
		MountPath: "/blob",
		Claim:     &types.VolumeClaim{Name: "shared-ns/blob"},
	})
	require.NoError(t, err)
	assert.Equal(t, "shared-ns", got.VolumeNamespace)
	assert.Equal(t, "blob", got.VolumeName)
}

func TestResolveVolumeMount_VolumeNotReady(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "default", "pending", "", types.VolumeStatusPending)

	svc := &types.Service{Name: "api", Namespace: "default"}
	_, err := ic.resolveVolumeMount(context.Background(), svc, types.VolumeMount{
		Name:      "x",
		MountPath: "/x",
		Claim:     &types.VolumeClaim{Name: "pending"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not ready")
}

func TestResolveVolumeMount_AvailableButNoHandle(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "default", "broken", "", types.VolumeStatusAvailable)

	svc := &types.Service{Name: "api", Namespace: "default"}
	_, err := ic.resolveVolumeMount(context.Background(), svc, types.VolumeMount{
		Name:      "x",
		MountPath: "/x",
		Claim:     &types.VolumeClaim{Name: "broken"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no handle")
}

func TestResolveVolumeMount_MissingVolume(t *testing.T) {
	_, ic := newInstanceControllerForVolumeTests(t)

	svc := &types.Service{Name: "api", Namespace: "default"}
	_, err := ic.resolveVolumeMount(context.Background(), svc, types.VolumeMount{
		Name:      "x",
		MountPath: "/x",
		Claim:     &types.VolumeClaim{Name: "ghost"},
	})
	require.Error(t, err)
}

func TestResolveVolumeMount_RejectsClaimTemplate(t *testing.T) {
	_, ic := newInstanceControllerForVolumeTests(t)

	svc := &types.Service{Name: "api", Namespace: "default"}
	_, err := ic.resolveVolumeMount(context.Background(), svc, types.VolumeMount{
		Name:          "x",
		MountPath:     "/x",
		ClaimTemplate: &types.VolumeClaimTemplate{Size: "1Gi"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "claimTemplate")
}

func TestResolveVolumeMount_RejectsBoth(t *testing.T) {
	_, ic := newInstanceControllerForVolumeTests(t)

	svc := &types.Service{Name: "api", Namespace: "default"}
	_, err := ic.resolveVolumeMount(context.Background(), svc, types.VolumeMount{
		Name:          "x",
		MountPath:     "/x",
		Claim:         &types.VolumeClaim{Name: "y"},
		ClaimTemplate: &types.VolumeClaimTemplate{Size: "1Gi"},
	})
	require.Error(t, err)
}

func TestResolveVolumeMount_RejectsNeither(t *testing.T) {
	_, ic := newInstanceControllerForVolumeTests(t)

	svc := &types.Service{Name: "api", Namespace: "default"}
	_, err := ic.resolveVolumeMount(context.Background(), svc, types.VolumeMount{
		Name:      "x",
		MountPath: "/x",
	})
	require.Error(t, err)
}
