package volumes

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDriver is a minimal driver that records every call. It implements
// the full driver.Driver surface; everything beyond Attach/Mount/Unmount/
// Detach returns ErrUnsupported because the agent subsystem never calls
// the controller-only methods.
type fakeDriver struct {
	name string

	mu sync.Mutex

	attachCalls   []driver.NodeID
	detachCalls   []driver.NodeID
	mountCalls    []driver.MountOpts
	unmountCalls  []driver.MountTarget
	attachErr     error
	mountErr      error
	rewriteTarget driver.MountTarget // when non-empty, Mount returns this instead of opts.Target
}

func (f *fakeDriver) Name() string                    { return f.name }
func (f *fakeDriver) Capabilities() driver.Capabilities { return driver.Capabilities{} }
func (f *fakeDriver) Provision(context.Context, driver.ProvisionRequest) (driver.VolumeHandle, error) {
	return "", driver.ErrUnsupported
}
func (f *fakeDriver) Delete(context.Context, driver.VolumeHandle) error { return nil }
func (f *fakeDriver) Attach(_ context.Context, _ driver.VolumeHandle, node driver.NodeID) (driver.DevicePath, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachCalls = append(f.attachCalls, node)
	if f.attachErr != nil {
		return "", f.attachErr
	}
	return "/dev/fake", nil
}
func (f *fakeDriver) Detach(_ context.Context, _ driver.VolumeHandle, node driver.NodeID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detachCalls = append(f.detachCalls, node)
	return nil
}
func (f *fakeDriver) Mount(_ context.Context, opts driver.MountOpts) (driver.MountTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mountCalls = append(f.mountCalls, opts)
	if f.mountErr != nil {
		return "", f.mountErr
	}
	if f.rewriteTarget != "" {
		return f.rewriteTarget, nil
	}
	return opts.Target, nil
}
func (f *fakeDriver) Unmount(_ context.Context, target driver.MountTarget) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmountCalls = append(f.unmountCalls, target)
	return nil
}
func (f *fakeDriver) Snapshot(context.Context, driver.SnapshotRequest) (driver.SnapshotHandle, error) {
	return "", driver.ErrUnsupported
}
func (f *fakeDriver) RestoreFromSnapshot(context.Context, driver.RestoreRequest) (driver.VolumeHandle, error) {
	return "", driver.ErrUnsupported
}
func (f *fakeDriver) Expand(context.Context, driver.VolumeHandle, string) error {
	return driver.ErrUnsupported
}

func (f *fakeDriver) snapshot() (attaches, detaches int, mounts int, unmounts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.attachCalls), len(f.detachCalls), len(f.mountCalls), len(f.unmountCalls)
}

// newSubsystem wires a Subsystem against a fake driver and an in-memory
// test store. Returns the subsystem, the store, and the driver so tests
// can stage events and assert on driver calls.
func newSubsystem(t *testing.T, nodeID string) (*Subsystem, *store.TestStore, *fakeDriver) {
	t.Helper()
	ts := store.NewTestStore()

	drv := &fakeDriver{name: "fake"}
	lookup := func(_ context.Context, vol *types.Volume) (driver.Driver, error) {
		if vol.StorageClassName != drv.name {
			return nil, errors.New("unknown driver: " + vol.StorageClassName)
		}
		return drv, nil
	}
	root := t.TempDir()
	sub, err := New(Config{
		Store:     ts,
		NodeID:    nodeID,
		Lookup:    lookup,
		MountRoot: root,
		Logger:    log.NewLogger(),
	})
	require.NoError(t, err)
	return sub, ts, drv
}

// waitFor polls fn until it returns nil or the deadline expires.
func waitFor(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := fn(); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitFor timed out: %v", lastErr)
}

// boundVolume builds a Volume in the shape the subsystem expects to mount.
func boundVolume(id, ns, name, nodeID string) *types.Volume {
	return &types.Volume{
		ID:               id,
		Name:             name,
		Namespace:        ns,
		StorageClassName: "fake",
		Handle:           "h-" + id,
		BoundNode:        nodeID,
		Status:           types.VolumeStatusBound,
		AccessMode:       types.AccessModeRWO,
		CreatedAt:        time.Now().UTC(),
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestNew_Validation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		err  string
	}{
		{"missing store", Config{NodeID: "n1", Lookup: func(context.Context, *types.Volume) (driver.Driver, error) { return nil, nil }}, "Store"},
		{"missing nodeID", Config{Store: store.NewTestStore(), Lookup: func(context.Context, *types.Volume) (driver.Driver, error) { return nil, nil }}, "NodeID"},
		{"missing lookup", Config{Store: store.NewTestStore(), NodeID: "n1"}, "Lookup"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(c.cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.err)
		})
	}
}

// TestSubsystem_AttachMountOnBind exercises the happy-path watch flow:
// a volume is created bound to this node and the subsystem responds with
// Attach + Mount, recording the result for MountTargetFor.
func TestSubsystem_AttachMountOnBind(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSubsystem(t, nodeID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	t.Cleanup(func() { _ = sub.Stop(context.Background()) })

	vol := boundVolume("v-1", "default", "data", nodeID)
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	expectedTarget := filepath.Join(sub.cfg.MountRoot, vol.ID)
	waitFor(t, 2*time.Second, func() error {
		got, ok := sub.MountTargetFor(vol.ID)
		if !ok {
			return errors.New("not yet mounted")
		}
		if got != expectedTarget {
			return errors.New("unexpected target: " + got)
		}
		return nil
	})

	a, d, m, u := drv.snapshot()
	assert.Equal(t, 1, a, "Attach called once")
	assert.Equal(t, 0, d)
	assert.Equal(t, 1, m, "Mount called once")
	assert.Equal(t, 0, u)
}

// TestSubsystem_IgnoresOtherNodes asserts that volumes bound to a
// different node are left alone.
func TestSubsystem_IgnoresOtherNodes(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSubsystem(t, nodeID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	t.Cleanup(func() { _ = sub.Stop(context.Background()) })

	vol := boundVolume("v-other", "default", "elsewhere", "node-B")
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	// Give the watch loop time to (not) act.
	time.Sleep(100 * time.Millisecond)
	_, ok := sub.MountTargetFor(vol.ID)
	assert.False(t, ok, "volume bound to another node must not be tracked")
	a, _, m, _ := drv.snapshot()
	assert.Zero(t, a)
	assert.Zero(t, m)
}

// TestSubsystem_IgnoresUnboundOrPending asserts that volumes that are
// not Available/Bound or have an empty handle are ignored.
func TestSubsystem_IgnoresUnboundOrPending(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSubsystem(t, nodeID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	t.Cleanup(func() { _ = sub.Stop(context.Background()) })

	pending := boundVolume("v-p", "default", "pending", nodeID)
	pending.Status = types.VolumeStatusPending
	noHandle := boundVolume("v-h", "default", "no-handle", nodeID)
	noHandle.Handle = ""

	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, "default", "pending", pending))
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, "default", "no-handle", noHandle))

	time.Sleep(100 * time.Millisecond)
	_, okP := sub.MountTargetFor(pending.ID)
	_, okH := sub.MountTargetFor(noHandle.ID)
	assert.False(t, okP)
	assert.False(t, okH)
	a, _, m, _ := drv.snapshot()
	assert.Zero(t, a)
	assert.Zero(t, m)
}

// TestSubsystem_UnmountDetachOnUnbind covers the inverse: a tracked
// volume whose BoundNode is cleared (e.g. operator detaches via
// `rune volume detach`) is unmounted + detached and dropped from the
// table.
func TestSubsystem_UnmountDetachOnUnbind(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSubsystem(t, nodeID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	t.Cleanup(func() { _ = sub.Stop(context.Background()) })

	vol := boundVolume("v-1", "default", "data", nodeID)
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))
	waitFor(t, 2*time.Second, func() error {
		if _, ok := sub.MountTargetFor(vol.ID); ok {
			return nil
		}
		return errors.New("not yet mounted")
	})

	// Detach: clear BoundNode + flip status back to Available.
	vol.BoundNode = ""
	vol.Status = types.VolumeStatusAvailable
	require.NoError(t, ts.Update(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 2*time.Second, func() error {
		if _, ok := sub.MountTargetFor(vol.ID); ok {
			return errors.New("still tracked")
		}
		return nil
	})

	a, d, m, u := drv.snapshot()
	assert.Equal(t, 1, a)
	assert.Equal(t, 1, d, "Detach called on unbind")
	assert.Equal(t, 1, m)
	assert.Equal(t, 1, u, "Unmount called on unbind")
}

// TestSubsystem_DeleteEvent covers the delete code path explicitly.
func TestSubsystem_DeleteEvent(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSubsystem(t, nodeID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	t.Cleanup(func() { _ = sub.Stop(context.Background()) })

	vol := boundVolume("v-1", "default", "data", nodeID)
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))
	waitFor(t, 2*time.Second, func() error {
		if _, ok := sub.MountTargetFor(vol.ID); ok {
			return nil
		}
		return errors.New("not yet mounted")
	})

	require.NoError(t, ts.Delete(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name))
	waitFor(t, 2*time.Second, func() error {
		if _, ok := sub.MountTargetFor(vol.ID); ok {
			return errors.New("still tracked")
		}
		return nil
	})
	_, d, _, u := drv.snapshot()
	assert.Equal(t, 1, d)
	assert.Equal(t, 1, u)
}

// TestSubsystem_StopUnmountsTracked asserts that Stop drains the
// in-memory tracker by calling Unmount + Detach for every tracked mount.
func TestSubsystem_StopUnmountsTracked(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSubsystem(t, nodeID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))

	vol := boundVolume("v-1", "default", "data", nodeID)
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))
	waitFor(t, 2*time.Second, func() error {
		if _, ok := sub.MountTargetFor(vol.ID); ok {
			return nil
		}
		return errors.New("not yet mounted")
	})

	require.NoError(t, sub.Stop(context.Background()))
	_, ok := sub.MountTargetFor(vol.ID)
	assert.False(t, ok, "Stop must drop tracked entries")
	_, d, _, u := drv.snapshot()
	assert.Equal(t, 1, d)
	assert.Equal(t, 1, u)
}

// TestSubsystem_AttachFailureLeavesUntracked verifies that an Attach
// failure does not record the volume in the mount table. Mount is
// never called.
func TestSubsystem_AttachFailureLeavesUntracked(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSubsystem(t, nodeID)
	drv.attachErr = errors.New("boom")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	t.Cleanup(func() { _ = sub.Stop(context.Background()) })

	vol := boundVolume("v-1", "default", "data", nodeID)
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	// Give the watch loop time to react.
	time.Sleep(100 * time.Millisecond)
	_, ok := sub.MountTargetFor(vol.ID)
	assert.False(t, ok)
	a, _, m, _ := drv.snapshot()
	assert.Equal(t, 1, a)
	assert.Zero(t, m, "Mount must not be called when Attach fails")
}

// TestSubsystem_MountFailureDetaches verifies that a Mount failure
// triggers a best-effort Detach so we don't leave a half-attached
// device behind.
func TestSubsystem_MountFailureDetaches(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSubsystem(t, nodeID)
	drv.mountErr = errors.New("mount-boom")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	t.Cleanup(func() { _ = sub.Stop(context.Background()) })

	vol := boundVolume("v-1", "default", "data", nodeID)
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 2*time.Second, func() error {
		_, _, _, _ = drv.snapshot()
		a, d, m, _ := drv.snapshot()
		if a >= 1 && m >= 1 && d >= 1 {
			return nil
		}
		return errors.New("not yet attempted")
	})
	_, ok := sub.MountTargetFor(vol.ID)
	assert.False(t, ok, "failed mount must not appear in the table")
}

// TestSubsystem_DriverRewritesTarget covers drivers (like local-host)
// that ignore the proposed target and return the underlying host path.
// The subsystem records what the driver returned, not what it asked for.
func TestSubsystem_DriverRewritesTarget(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSubsystem(t, nodeID)
	drv.rewriteTarget = "/srv/operator-owned"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	t.Cleanup(func() { _ = sub.Stop(context.Background()) })

	vol := boundVolume("v-1", "default", "data", nodeID)
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 2*time.Second, func() error {
		got, ok := sub.MountTargetFor(vol.ID)
		if !ok {
			return errors.New("not yet mounted")
		}
		if got != "/srv/operator-owned" {
			return errors.New("driver-rewritten target not honoured: " + got)
		}
		return nil
	})
}
