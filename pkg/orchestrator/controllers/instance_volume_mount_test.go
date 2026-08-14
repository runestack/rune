package controllers

import (
	"context"
	"errors"
	"strings"
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
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := ic.resolveVolumeMount(ctx, svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "x",
		MountPath: "/x",
		Claim:     &types.VolumeClaim{Name: "pending"},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "not ready"),
		"unexpected error: %v", err)
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
	assert.Contains(t, err.Error(), "no mount source")
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
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := ic.resolveVolumeMount(ctx, svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
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
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "not ready"),
		"unexpected error: %v", err)

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

// TestResolveVolumeMount_ClaimTemplateUsesOrdinalNotName proves the per-replica
// volume binds to the instance's Ordinal field, NOT to anything parsed out of
// the instance name. The name here is deliberately non-ordinal ("api-x7f2k",
// the {service}-{shorthash} shape #84 moves to), yet the resolved volume must
// still be data-api-2 because Ordinal is 2.
func TestResolveVolumeMount_ClaimTemplateUsesOrdinalNotName(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)
	pre := &types.Volume{
		ID:               "data-api-2",
		Name:             "data-api-2",
		Namespace:        "default",
		StorageClassName: "fast",
		Size:             "5Gi",
		AccessMode:       types.AccessModeRWO,
		ReclaimPolicy:    types.ReclaimPolicyDelete,
		OwnerService:     "default/api",
		BoundClaim:       "api/data/2",
		Status:           types.VolumeStatusAvailable,
		Handle:           "/var/lib/rune/volumes/default/data-api-2",
		CreatedAt:        time.Now().UTC().Add(-time.Hour),
	}
	require.NoError(t, ts.Create(context.Background(), types.ResourceTypeVolume, "default", "data-api-2", pre))

	svc := &types.Service{Name: "api", Namespace: "default"}
	got, err := ic.resolveVolumeMount(context.Background(), svc,
		&types.Instance{Name: "api-x7f2k", Ordinal: 2}, types.VolumeMount{
			Name:      "data",
			MountPath: "/data",
			ClaimTemplate: &types.VolumeClaimTemplate{
				StorageClassName: "fast",
				Size:             "5Gi",
				AccessMode:       types.AccessModeRWO,
			},
		})
	require.NoError(t, err)
	assert.Equal(t, "data-api-2", got.VolumeName, "binding follows Ordinal, not the name")
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

// When the controller has a nodeID wired (production runed) and the
// volume is not yet bound to any node, resolveVolumeMount stamps
// BoundNode + BoundClaim on the volume so the agent-side Subsystem
// will pick it up and call driver.Attach + Mount. This is the trigger
// that turns a freshly-provisioned cloud volume into a real on-host
// mount target.
func TestResolveVolumeMount_StampsBoundNode(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "default", "data", "do-vol-abc123", types.VolumeStatusAvailable)
	ic.nodeID = "node-a"
	ic.SetMountResolver(&fakeMountResolver{targets: map[string]string{
		"vol-data": "/var/lib/rune/mounts/vol-data",
	}})

	svc := &types.Service{Name: "api", Namespace: "default"}
	_, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "data",
		MountPath: "/data",
		Claim:     &types.VolumeClaim{Name: "data"},
	})
	require.NoError(t, err)

	var v types.Volume
	require.NoError(t, ts.Get(context.Background(), types.ResourceTypeVolume, "default", "data", &v))
	assert.Equal(t, "node-a", v.BoundNode, "BoundNode must be stamped so the agent Subsystem mounts this volume")
	assert.Equal(t, "default/api-0", v.BoundClaim, "BoundClaim records which instance bound the volume")
}

// On `rune restart` the service cycles 1→0→1; the new instance keeps
// the same instance name but is a fresh row. BoundClaim must be
// refreshed so the volume row doesn't keep pointing at the previous
// (Deleted) instance, otherwise rune volume get and any future
// detach/delete logic that keys on BoundClaim sees stale state.
// BoundNode stays unchanged (still this node) — restart deliberately
// keeps the mount alive across the 1→0→1 to make the second leg fast.
func TestResolveVolumeMount_RefreshesBoundClaimOnRestart(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "default", "data", "do-vol-abc123", types.VolumeStatusAvailable)

	// Seed the volume as already bound to a previous instance on this
	// node — the post-restart-first-reconcile state.
	var v types.Volume
	require.NoError(t, ts.Get(context.Background(), types.ResourceTypeVolume, "default", "data", &v))
	v.BoundNode = "node-a"
	v.BoundClaim = "default/api-0" // stale: that instance was Deleted
	require.NoError(t, ts.Update(context.Background(), types.ResourceTypeVolume, "default", "data", &v))

	ic.nodeID = "node-a"
	ic.SetMountResolver(&fakeMountResolver{targets: map[string]string{
		"vol-data": "/var/lib/rune/mounts/vol-data",
	}})

	// New instance with the same name (api-0 again, but a different
	// row in practice — only the name is shared).
	svc := &types.Service{Name: "api", Namespace: "default"}
	_, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "data",
		MountPath: "/data",
		Claim:     &types.VolumeClaim{Name: "data"},
	})
	require.NoError(t, err)

	require.NoError(t, ts.Get(context.Background(), types.ResourceTypeVolume, "default", "data", &v))
	assert.Equal(t, "node-a", v.BoundNode, "BoundNode stays on the same node across restart")
	assert.Equal(t, "default/api-0", v.BoundClaim, "BoundClaim refreshed to the new consuming instance")
}

// When a MountResolver is wired but has no entry for this volume yet
// (typical race: bind happened, mount has not), resolveVolumeMount
// returns a transient error so the service reconciler retries. Falling
// back to Volume.Handle would be wrong for any driver where Handle is
// not a host path (do-volume, future cloud drivers); for the in-tree
// local / local-host drivers, the agent-side Subsystem will populate
// the resolver shortly anyway.
func TestResolveVolumeMount_ResolverMissReturnsTransientError(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "default", "data", "/var/lib/rune/volumes/default/data", types.VolumeStatusAvailable)
	ic.SetMountResolver(&fakeMountResolver{targets: map[string]string{}})

	svc := &types.Service{Name: "api", Namespace: "default"}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := ic.resolveVolumeMount(ctx, svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "data",
		MountPath: "/data",
		Claim:     &types.VolumeClaim{Name: "data"},
	})
	require.Error(t, err)
	// Short ctx: returns deadline exceeded; full timeout: "not yet mounted".
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "not yet mounted"),
		"unexpected error: %v", err)
}

// With no MountResolver wired (dev/standalone, tests), Volume.Handle
// remains the bind source. This preserves correctness for the in-tree
// local / local-host drivers in setups that haven't plumbed the
// agent-side Subsystem.
func TestResolveVolumeMount_NoResolverUsesHandle(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "default", "data", "/var/lib/rune/volumes/default/data", types.VolumeStatusAvailable)

	svc := &types.Service{Name: "api", Namespace: "default"}
	got, err := ic.resolveVolumeMount(context.Background(), svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "data",
		MountPath: "/data",
		Claim:     &types.VolumeClaim{Name: "data"},
	})
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/rune/volumes/default/data", got.Source)
}

// shortenMountWait shrinks waitForMountTarget's bounds for the duration of a
// test so the "gave up waiting" branch is reachable without a 45s sleep.
func shortenMountWait(t *testing.T) {
	t.Helper()
	oldTimeout, oldPoll := mountTargetTimeout, mountTargetPollInterval
	mountTargetTimeout, mountTargetPollInterval = 100*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		mountTargetTimeout, mountTargetPollInterval = oldTimeout, oldPoll
	})
}

// reportingMountResolver is a MountResolver that also implements the optional
// MountErrorReporter, mimicking the agent's volumes Subsystem.
type reportingMountResolver struct {
	targets map[string]string
	errs    map[string]string
}

func (r *reportingMountResolver) MountTargetFor(volumeID string) (string, bool) {
	t, ok := r.targets[volumeID]
	return t, ok
}

func (r *reportingMountResolver) MountErrorFor(volumeID string) (string, bool) {
	e, ok := r.errs[volumeID]
	return e, ok
}

// TestResolveVolumeMount_SurfacesAgentMountError is the #169 regression: when
// the agent knows why a volume never mounted, the instance error must say so.
// A DigitalOcean token expired across a droplet reboot; the volumes were
// attached and mounted the whole time, but the instance only ever reported
// "not yet mounted (will retry)" — the HTTP 401 lived solely in the agent's
// startup log, so the failure was undiagnosable from the CLI.
func TestResolveVolumeMount_SurfacesAgentMountError(t *testing.T) {
	shortenMountWait(t)
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "default", "data", "/var/lib/rune/volumes/default/data", types.VolumeStatusAvailable)

	const cause = "attach default/data: storage driver: provider rejected credentials: " +
		"GET /v2/volumes/abc -> HTTP 401 (check the storage class's apiToken)"
	ic.SetMountResolver(&reportingMountResolver{
		targets: map[string]string{},
		// keyed by volumeMountKey(), which prefers Volume.ID
		errs: map[string]string{"vol-data": cause},
	})

	svc := &types.Service{Name: "api", Namespace: "default"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := ic.resolveVolumeMount(ctx, svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "data",
		MountPath: "/data",
		Claim:     &types.VolumeClaim{Name: "data"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 401",
		"the operator-facing error must carry the driver's cause, not just 'not yet mounted'")
	assert.Contains(t, err.Error(), "apiToken",
		"the error should point at the credential to fix")
	assert.NotContains(t, err.Error(), "will retry",
		"a terminal credential failure must not be described as a transient retry")
}

// A resolver that does NOT implement MountErrorReporter (or has no recorded
// cause) keeps the original transient message — the optional interface must
// not change behaviour for implementations that don't provide it.
func TestResolveVolumeMount_NoReporterKeepsTransientMessage(t *testing.T) {
	shortenMountWait(t)
	ts, ic := newInstanceControllerForVolumeTests(t)
	putVolume(t, ts, "default", "data", "/var/lib/rune/volumes/default/data", types.VolumeStatusAvailable)
	ic.SetMountResolver(&fakeMountResolver{targets: map[string]string{}})

	svc := &types.Service{Name: "api", Namespace: "default"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := ic.resolveVolumeMount(ctx, svc, &types.Instance{Name: "api-0"}, types.VolumeMount{
		Name:      "data",
		MountPath: "/data",
		Claim:     &types.VolumeClaim{Name: "data"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet mounted")
}
