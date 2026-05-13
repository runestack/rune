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
	got, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
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
	got, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
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
	got, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
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
	_, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
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
	_, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
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
	_, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "x",
		MountPath: "/x",
		Claim:     &types.VolumeClaim{Name: "ghost"},
	})
	require.Error(t, err)
}

func TestResolveVolumeMount_ClaimTemplateAutoProvisions(t *testing.T) {
	// First reconcile: volume row does not yet exist. ensureClaimTemplateVolume
	// creates it in Pending state and the resolver reports "not ready" so
	// the service reconciler will retry. We then assert the row was
	// stamped with the expected ownership/binding metadata.
	ts, ic := newInstanceControllerForVolumeTests(t)
	svc := &types.Service{Name: "api", Namespace: "default"}
	_, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "data",
		MountPath: "/data",
		ClaimTemplate: &types.VolumeClaimTemplate{
			StorageClassName: "fast",
			Size:             "5Gi",
			AccessMode:       types.AccessModeRWO,
			ReclaimPolicy:    types.ReclaimPolicyDelete,
		},
	})
	require.Error(t, err, "first call returns 'not ready' so reconciler retries")
	assert.Contains(t, err.Error(), "not ready")

	var v types.Volume
	require.NoError(t, ts.Get(context.Background(), types.ResourceTypeVolume, "default", "data-api-0", &v))
	assert.Equal(t, "fast", v.StorageClassName)
	assert.Equal(t, "5Gi", v.Size)
	assert.Equal(t, types.AccessModeRWO, v.AccessMode)
	assert.Equal(t, types.ReclaimPolicyDelete, v.ReclaimPolicy)
	assert.Equal(t, "default/api", v.OwnerService)
	assert.Equal(t, "api/data/0", v.BoundClaim)
	assert.Equal(t, types.VolumeStatusPending, v.Status)
}

func TestResolveVolumeMount_ClaimTemplateIdempotent(t *testing.T) {
	// Second reconcile (volume already Available): resolver returns
	// the runner-facing mount; the existing row is not overwritten.
	ts, ic := newInstanceControllerForVolumeTests(t)
	pre := &types.Volume{
		ID:               "data-api-0",
		Name:             "data-api-0",
		Namespace:        "default",
		StorageClassName: "fast",
		Size:             "5Gi",
		AccessMode:       types.AccessModeRWO,
		ReclaimPolicy:    types.ReclaimPolicyDelete,
		OwnerService:     "default/api",
		BoundClaim:       "api/data/0",
		Status:           types.VolumeStatusAvailable,
		Handle:           "/var/lib/rune/volumes/default/data-api-0",
		CreatedAt:        time.Now().UTC().Add(-time.Hour),
	}
	require.NoError(t, ts.Create(context.Background(), types.ResourceTypeVolume, "default", "data-api-0", pre))

	svc := &types.Service{Name: "api", Namespace: "default"}
	got, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "data",
		MountPath: "/data",
		ClaimTemplate: &types.VolumeClaimTemplate{
			StorageClassName: "fast",
			Size:             "5Gi",
			AccessMode:       types.AccessModeRWO,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "data-api-0", got.VolumeName)
	assert.Equal(t, "/var/lib/rune/volumes/default/data-api-0", got.Source)
}

func TestResolveVolumeMount_ClaimTemplateUnparseableInstanceName(t *testing.T) {
	_, ic := newInstanceControllerForVolumeTests(t)
	svc := &types.Service{Name: "api", Namespace: "default"}
	_, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "ad-hoc"}, types.VolumeMount{
		Name:          "data",
		MountPath:     "/data",
		ClaimTemplate: &types.VolumeClaimTemplate{Size: "1Gi"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ordinal")
}

func TestResolveVolumeMount_RejectsBoth(t *testing.T) {
	_, ic := newInstanceControllerForVolumeTests(t)

	svc := &types.Service{Name: "api", Namespace: "default"}
	_, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
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
	_, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "x",
		MountPath: "/x",
	})
	require.Error(t, err)
}

// fakeMountResolver is a tiny MountResolver used to test the
// resolver-first / Volume.Handle-fallback policy in resolveVolumeMount.
type fakeMountResolver struct {
	targets map[string]string
}

func (f *fakeMountResolver) MountTargetFor(volumeID string) (string, bool) {
	t, ok := f.targets[volumeID]
	return t, ok
}

// When a MountResolver is wired and reports a target, that target wins
// over Volume.Handle. This is the path that makes future block-device
// drivers (do-volume, ...) usable, since for those Volume.Handle is a
// cloud-side identifier rather than a host filesystem path.
func TestResolveVolumeMount_ResolverPreferredOverHandle(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "default", "data", "do-vol-abc123", types.VolumeStatusAvailable)
	ic.SetMountResolver(&fakeMountResolver{targets: map[string]string{
		"vol-data": "/var/lib/rune/mounts/vol-data",
	}})

	svc := &types.Service{Name: "api", Namespace: "default"}
	got, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "data",
		MountPath: "/data",
		Claim:     &types.VolumeClaim{Name: "data"},
	})
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/rune/mounts/vol-data", got.Source,
		"resolver-reported target must win over Volume.Handle")
}

// When a MountResolver is wired but has no entry for this volume yet
// (typical race: bind happened, mount has not), the resolver returns
// (= , false) and we fall back to Volume.Handle. This preserves
// correctness for the in-tree local / local-host drivers (their Mount
// returns the host path verbatim, so target == handle).
func TestResolveVolumeMount_ResolverMissFallsBackToHandle(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "default", "data", "/var/lib/rune/volumes/default/data", types.VolumeStatusAvailable)
	ic.SetMountResolver(&fakeMountResolver{targets: map[string]string{}})

	svc := &types.Service{Name: "api", Namespace: "default"}
	got, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "data",
		MountPath: "/data",
		Claim:     &types.VolumeClaim{Name: "data"},
	})
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/rune/volumes/default/data", got.Source,
		"with no resolver hit we must fall back to Volume.Handle")
}
