package volumes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/storage/driverparams"
	"github.com/runestack/rune/pkg/storage/mountinfo"
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

func (f *fakeDriver) Name() string                      { return f.name }
func (f *fakeDriver) Capabilities() driver.Capabilities { return driver.Capabilities{} }
func (f *fakeDriver) Provision(context.Context, driver.OpContext, driver.ProvisionRequest) (driver.VolumeHandle, error) {
	return "", driver.ErrUnsupported
}
func (f *fakeDriver) Delete(context.Context, driver.OpContext, driver.VolumeHandle) error {
	return nil
}
func (f *fakeDriver) Attach(_ context.Context, _ driver.OpContext, handle driver.VolumeHandle, node driver.NodeID) (driver.DevicePath, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attachCalls = append(f.attachCalls, node)
	if f.attachErr != nil {
		return "", f.attachErr
	}
	return driver.DevicePath("/dev/" + handle), nil
}
func (f *fakeDriver) Detach(_ context.Context, _ driver.OpContext, _ driver.VolumeHandle, node driver.NodeID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detachCalls = append(f.detachCalls, node)
	return nil
}
func (f *fakeDriver) Mount(_ context.Context, _ driver.OpContext, opts driver.MountOpts) (driver.MountTarget, error) {
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
func (f *fakeDriver) Unmount(_ context.Context, _ driver.OpContext, target driver.MountTarget) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmountCalls = append(f.unmountCalls, target)
	return nil
}
func (f *fakeDriver) Snapshot(context.Context, driver.OpContext, driver.SnapshotRequest) (driver.SnapshotHandle, error) {
	return "", driver.ErrUnsupported
}
func (f *fakeDriver) RestoreFromSnapshot(context.Context, driver.OpContext, driver.RestoreRequest) (driver.VolumeHandle, error) {
	return "", driver.ErrUnsupported
}
func (f *fakeDriver) Expand(context.Context, driver.OpContext, driver.VolumeHandle, string) error {
	return driver.ErrUnsupported
}
func (f *fakeDriver) DeleteSnapshot(context.Context, driver.OpContext, driver.SnapshotHandle) error {
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

	expectedTarget := filepath.Join(sub.cfg.MountRoot, vol.Namespace, vol.ID)
	waitFor(t, 2*time.Second, func() error {
		got, ok := sub.MountTargetFor(vol.MountKey())
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
	_, ok := sub.MountTargetFor(vol.MountKey())
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
	_, okP := sub.MountTargetFor(pending.MountKey())
	_, okH := sub.MountTargetFor(noHandle.MountKey())
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
		if _, ok := sub.MountTargetFor(vol.MountKey()); ok {
			return nil
		}
		return errors.New("not yet mounted")
	})

	// Detach: clear BoundNode + flip status back to Available.
	vol.BoundNode = ""
	vol.Status = types.VolumeStatusAvailable
	require.NoError(t, ts.Update(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	// Unbind clears the tracker before tearDown's Unmount+Detach, so wait
	// on the driver counts we assert rather than MountTargetFor (which
	// flips as soon as the tracker entry is removed, ahead of teardown).
	waitFor(t, 2*time.Second, func() error {
		if _, d, _, u := drv.snapshot(); d != 1 || u != 1 {
			return errors.New("waiting for Unmount+Detach")
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
		if _, ok := sub.MountTargetFor(vol.MountKey()); ok {
			return nil
		}
		return errors.New("not yet mounted")
	})

	require.NoError(t, ts.Delete(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name))
	// The delete handler clears the tracker (so MountTargetFor returns
	// false) BEFORE running tearDown's Unmount+Detach. Waiting on
	// MountTargetFor therefore races teardown — the driver calls may not
	// have landed yet. Wait on the driver counts we actually assert.
	waitFor(t, 2*time.Second, func() error {
		if _, d, _, u := drv.snapshot(); d != 1 || u != 1 {
			return errors.New("waiting for Unmount+Detach")
		}
		return nil
	})
	_, d, _, u := drv.snapshot()
	assert.Equal(t, 1, d, "Detach called on delete")
	assert.Equal(t, 1, u, "Unmount called on delete")
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
		if _, ok := sub.MountTargetFor(vol.MountKey()); ok {
			return nil
		}
		return errors.New("not yet mounted")
	})

	require.NoError(t, sub.Stop(context.Background()))
	_, ok := sub.MountTargetFor(vol.MountKey())
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
	_, ok := sub.MountTargetFor(vol.MountKey())
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
	_, ok := sub.MountTargetFor(vol.MountKey())
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
		got, ok := sub.MountTargetFor(vol.MountKey())
		if !ok {
			return errors.New("not yet mounted")
		}
		if got != "/srv/operator-owned" {
			return errors.New("driver-rewritten target not honoured: " + got)
		}
		return nil
	})
}

// TestSubsystem_RecordsAndClearsMountError covers the #169 plumbing: when a
// volume fails to come up, the subsystem must remember WHY so the
// orchestrator can put the real cause on the blocked instance instead of a
// bare "not yet mounted (will retry)". The reason must clear once the volume
// mounts, so a recovered volume doesn't keep reporting a stale failure.
func TestSubsystem_RecordsAndClearsMountError(t *testing.T) {
	s, st, drv := newSubsystem(t, "node-a")
	ctx := context.Background()

	drv.attachErr = errors.New("storage driver: provider rejected credentials: HTTP 401")

	vol := &types.Volume{
		ID: "vol-1", Name: "data", Namespace: "default",
		StorageClassName: "fake",
		Handle:           "h-1", BoundNode: "node-a", Status: types.VolumeStatusBound,
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	// Failing bring-up records the cause.
	require.Error(t, s.reconcile(ctx, vol))
	cause, ok := s.MountErrorFor("default/vol-1")
	require.True(t, ok, "a failed bring-up must record its cause")
	require.Contains(t, cause, "HTTP 401")

	// Nothing is mounted, so the resolver still reports absent.
	_, mounted := s.MountTargetFor("default/vol-1")
	require.False(t, mounted)

	// Once the underlying problem is fixed, the next reconcile clears it.
	drv.attachErr = nil
	require.NoError(t, s.reconcile(ctx, vol))
	if _, still := s.MountErrorFor("default/vol-1"); still {
		t.Fatal("a successful bring-up must clear the recorded failure")
	}
	if _, mounted := s.MountTargetFor("default/vol-1"); !mounted {
		t.Fatal("volume should be tracked as mounted after a successful bring-up")
	}
}

// A volume that never failed has no recorded cause, so the orchestrator keeps
// its ordinary transient wording.
func TestSubsystem_NoMountErrorForHealthyVolume(t *testing.T) {
	s, _, _ := newSubsystem(t, "node-a")
	if _, ok := s.MountErrorFor("never-seen"); ok {
		t.Fatal("unknown volume must not report a mount error")
	}
}

// ---------------------------------------------------------------------------
// Teardown: concurrency + credential freshness (issues #185, #186)
// ---------------------------------------------------------------------------

// slowDriver blocks in Detach so a sequential drain is distinguishable
// from a concurrent one, and records the parameters each call was given.
type slowDriver struct {
	fakeDriver
	detachDelay time.Duration
	unmountErr  error

	pmu         sync.Mutex
	detachParam []map[string]string
	detachDL    []deadlineObs
}

// deadlineObs records whether the ctx a driver call received carried a
// deadline, and how far out it was.
type deadlineObs struct {
	set    bool
	within time.Duration
}

func (s *slowDriver) Detach(ctx context.Context, opctx driver.OpContext, h driver.VolumeHandle, node driver.NodeID) error {
	s.pmu.Lock()
	copied := map[string]string{}
	for k, v := range opctx.Parameters {
		copied[k] = v
	}
	s.detachParam = append(s.detachParam, copied)
	obs := deadlineObs{}
	if dl, ok := ctx.Deadline(); ok {
		obs.set = true
		obs.within = time.Until(dl)
	}
	s.detachDL = append(s.detachDL, obs)
	s.pmu.Unlock()
	if s.detachDelay > 0 {
		select {
		case <-time.After(s.detachDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.fakeDriver.Detach(ctx, opctx, h, node)
}

func (s *slowDriver) Unmount(ctx context.Context, opctx driver.OpContext, target driver.MountTarget) error {
	if s.unmountErr != nil {
		return s.unmountErr
	}
	return s.fakeDriver.Unmount(ctx, opctx, target)
}

func (s *slowDriver) detachDeadlines() []deadlineObs {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	out := make([]deadlineObs, len(s.detachDL))
	copy(out, s.detachDL)
	return out
}

func (s *slowDriver) detachParams() []map[string]string {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	out := make([]map[string]string, len(s.detachParam))
	copy(out, s.detachParam)
	return out
}

// newSlowSubsystem is newSubsystem with a driver whose Detach blocks, and
// an optional secret resolver.
func newSlowSubsystem(t *testing.T, nodeID string, delay time.Duration, lookupSecret driverparams.SecretLookup) (*Subsystem, *store.TestStore, *slowDriver) {
	t.Helper()
	ts := store.NewTestStore()
	drv := &slowDriver{fakeDriver: fakeDriver{name: "fake"}, detachDelay: delay}
	sub, err := New(Config{
		Store:  ts,
		NodeID: nodeID,
		Lookup: func(_ context.Context, vol *types.Volume) (driver.Driver, error) {
			if vol.StorageClassName != "fake" {
				return nil, errors.New("unknown driver")
			}
			return drv, nil
		},
		SecretLookup: lookupSecret,
		MountRoot:    t.TempDir(),
		Logger:       log.NewLogger(),
	})
	require.NoError(t, err)
	return sub, ts, drv
}

// mountN creates n bound volumes and waits for all of them to be mounted.
func mountN(t *testing.T, sub *Subsystem, ts *store.TestStore, ctx context.Context, nodeID string, n int) []*types.Volume {
	t.Helper()
	var vols []*types.Volume
	for i := 0; i < n; i++ {
		v := boundVolume(fmt.Sprintf("v-%d", i), "default", fmt.Sprintf("data-%d", i), nodeID)
		require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, v.Namespace, v.Name, v))
		vols = append(vols, v)
	}
	for _, v := range vols {
		id := v.MountKey()
		waitFor(t, 3*time.Second, func() error {
			if _, ok := sub.MountTargetFor(id); ok {
				return nil
			}
			return errors.New("not yet mounted: " + id)
		})
	}
	return vols
}

// TestSubsystem_StopDrainsConcurrently is the regression for issue #185.
// Teardown used to run sequentially against the single agent-shutdown
// budget, so the first slow detach could consume all of it: on a
// three-volume node exactly one volume was released and the rest failed
// with "context deadline exceeded". Draining in parallel means the wall
// clock is one detach, not three.
func TestSubsystem_StopDrainsConcurrently(t *testing.T) {
	const nodeID = "node-A"
	const detachDelay = 300 * time.Millisecond
	sub, ts, drv := newSlowSubsystem(t, nodeID, detachDelay, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	vols := mountN(t, sub, ts, ctx, nodeID, 3)

	start := time.Now()
	require.NoError(t, sub.Stop(context.Background()))
	elapsed := time.Since(start)

	_, d, _, _ := drv.snapshot()
	assert.Equal(t, len(vols), d, "every tracked volume must be detached")

	// Sequential would be >= 3x the delay. Allow generous headroom for
	// slow CI while still failing on a sequential drain.
	if elapsed >= 2*detachDelay {
		t.Errorf("drain took %s for %d volumes at %s each — looks sequential, not concurrent",
			elapsed, len(vols), detachDelay)
	}
}

// TestSubsystem_StopDrainsWithinTightBudget: with a shutdown budget
// smaller than the total sequential cost but larger than a single
// detach, every volume must still be released. This is the production
// shape from #185 (3 volumes, one shared budget, slow provider calls).
//
// Named for what it proves. It was originally called
// ...PerVolumeDeadline, but review showed it passed with the per-volume
// deadline removed entirely — because `WithTimeout(ctx, timeUntil(ctx
// deadline))` is just the parent deadline. The deadline split was
// inert; concurrency is what makes this pass, and the code no longer
// pretends otherwise.
func TestSubsystem_StopDrainsWithinTightBudget(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSlowSubsystem(t, nodeID, 200*time.Millisecond, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	vols := mountN(t, sub, ts, ctx, nodeID, 3)

	// 500ms: too small for 3x200ms sequentially, ample concurrently.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer stopCancel()
	require.NoError(t, sub.Stop(stopCtx))

	_, d, _, _ := drv.snapshot()
	assert.Equal(t, len(vols), d,
		"all volumes must detach within a budget that only fits one sequentially")
}

// TestSubsystem_StopSkipsTeardownWhenBudgetSpent: an already-expired
// shutdown context means every driver call would fail instantly. Say so
// once rather than emitting a failure per volume.
func TestSubsystem_StopSkipsTeardownWhenBudgetSpent(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSlowSubsystem(t, nodeID, 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	mountN(t, sub, ts, ctx, nodeID, 2)

	expired, expCancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer expCancel()
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, sub.Stop(expired))

	_, d, _, _ := drv.snapshot()
	assert.Zero(t, d, "no detach should be attempted once the budget is spent")
}

// TestSubsystem_TeardownReresolvesSecrets is the regression for issue
// #186. Detach used to reuse the OpContext captured at bring-up, whose
// secret refs were already resolved to plaintext — so a rotated
// credential never reached Detach and the provider rejected the stale
// value. Rotating again could not fix it.
func TestSubsystem_TeardownReresolvesSecrets(t *testing.T) {
	const nodeID = "node-A"

	// A resolver whose answer changes, standing in for `rune secret set`.
	var mu sync.Mutex
	token := "old-token"
	lookup := func(_ context.Context, ns, name, key string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return token, nil
	}

	sub, ts, drv := newSlowSubsystem(t, nodeID, 0, lookup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))

	// The class carries the secret ref, exactly as do-lon1 does.
	class := &types.StorageClass{
		Name:       "fake",
		Driver:     "fake",
		Parameters: map[string]string{"apiToken": "secret:tok.default.rune/token"},
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeStorageClass, "", class.Name, class))

	vol := boundVolume("v-1", "default", "data", nodeID)
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))
	waitFor(t, 3*time.Second, func() error {
		if _, ok := sub.MountTargetFor(vol.MountKey()); ok {
			return nil
		}
		return errors.New("not yet mounted")
	})

	// Rotate after the mount is established — the situation that made a
	// correct rotation look like a bad token.
	mu.Lock()
	token = "new-token"
	mu.Unlock()

	require.NoError(t, sub.Stop(context.Background()))

	params := drv.detachParams()
	require.Len(t, params, 1, "expected exactly one Detach")
	assert.Equal(t, "new-token", params[0]["apiToken"],
		"Detach must use the rotated credential, not the one resolved at mount time")
}

// TestSubsystem_TeardownFallsBackWhenResolveFails: teardown runs while
// the process is going away, so a failing secret lookup must not turn a
// stale credential into no credential at all.
func TestSubsystem_TeardownFallsBackWhenResolveFails(t *testing.T) {
	const nodeID = "node-A"

	var fail atomic.Bool
	lookup := func(_ context.Context, ns, name, key string) (string, error) {
		if fail.Load() {
			return "", errors.New("secret store unavailable")
		}
		return "mount-time-token", nil
	}

	sub, ts, drv := newSlowSubsystem(t, nodeID, 0, lookup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))

	class := &types.StorageClass{
		Name:       "fake",
		Driver:     "fake",
		Parameters: map[string]string{"apiToken": "secret:tok.default.rune/token"},
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeStorageClass, "", class.Name, class))
	vol := boundVolume("v-1", "default", "data", nodeID)
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))
	waitFor(t, 3*time.Second, func() error {
		if _, ok := sub.MountTargetFor(vol.MountKey()); ok {
			return nil
		}
		return errors.New("not yet mounted")
	})

	fail.Store(true) // the lookup breaks only for teardown
	require.NoError(t, sub.Stop(context.Background()))

	params := drv.detachParams()
	require.Len(t, params, 1)
	assert.Equal(t, "mount-time-token", params[0]["apiToken"],
		"a failed re-resolve must fall back to the credential captured at mount time")
	_, d, _, _ := drv.snapshot()
	assert.Equal(t, 1, d, "detach must still be attempted")
}

// TestSubsystem_StopBoundsDeadlinelessCaller covers the one thing the
// teardown timeout still does after review removed the inert per-volume
// split: a caller that supplies no deadline of its own must not let a
// driver's HTTP timeout (30s for dovolume) decide how long shutdown
// takes. Agent.Stop's Start-rollback path passes context.Background(),
// so this is reachable, not theoretical.
func TestSubsystem_StopBoundsDeadlinelessCaller(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSlowSubsystem(t, nodeID, 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	mountN(t, sub, ts, ctx, nodeID, 1)

	// No deadline on the caller's context.
	require.NoError(t, sub.Stop(context.Background()))

	deadlines := drv.detachDeadlines()
	require.Len(t, deadlines, 1)
	require.True(t, deadlines[0].set,
		"a deadline-less caller must still get a bounded teardown context")
	assert.LessOrEqual(t, deadlines[0].within, teardownFallbackTimeout,
		"the imposed bound must be teardownFallbackTimeout")
}

// TestSubsystem_StopHonoursCallerDeadline is the complement: when the
// caller *does* supply a deadline, teardown uses it as-is rather than
// substituting its own.
func TestSubsystem_StopHonoursCallerDeadline(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSlowSubsystem(t, nodeID, 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	mountN(t, sub, ts, ctx, nodeID, 1)

	callerBudget := 2 * time.Second
	stopCtx, stopCancel := context.WithTimeout(context.Background(), callerBudget)
	defer stopCancel()
	require.NoError(t, sub.Stop(stopCtx))

	deadlines := drv.detachDeadlines()
	require.Len(t, deadlines, 1)
	require.True(t, deadlines[0].set)
	assert.LessOrEqual(t, deadlines[0].within, callerBudget,
		"teardown must not extend past the caller's deadline")
	assert.Greater(t, deadlines[0].within, callerBudget-time.Second,
		"teardown must not shrink the caller's budget either")
}

// TestSubsystem_StopSkipsTeardownWhenCancelled: the guard covers a
// cancelled context, not only an expired deadline.
func TestSubsystem_StopSkipsTeardownWhenCancelled(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSlowSubsystem(t, nodeID, 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	mountN(t, sub, ts, ctx, nodeID, 2)

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	require.NoError(t, sub.Stop(cancelled))

	_, d, _, _ := drv.snapshot()
	assert.Zero(t, d, "a cancelled context must skip teardown, like an expired one")
}

// TestSubsystem_DetachedCountExcludesUnmountOnlyFailures is the
// regression for the summary line. It reports how many volumes are off
// this node, which is not the same as how many teardowns returned nil:
// Unmount can fail while Detach succeeds. Counting the error would call
// a detached volume "still attached" — the exact question the line was
// added to answer.
func TestSubsystem_DetachedCountExcludesUnmountOnlyFailures(t *testing.T) {
	const nodeID = "node-A"
	sub, ts, drv := newSlowSubsystem(t, nodeID, 0, nil)
	drv.unmountErr = errors.New("target busy")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, sub.Start(ctx))
	vol := boundVolume("v-1", "default", "data", nodeID)
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))
	waitFor(t, 3*time.Second, func() error {
		if _, ok := sub.MountTargetFor(vol.MountKey()); ok {
			return nil
		}
		return errors.New("not yet mounted")
	})

	// tearDown returns the unmount error, but the volume IS detached.
	detached, err := sub.tearDown(context.Background(), vol.ID, func() trackedMount {
		sub.stateMu.RLock()
		defer sub.stateMu.RUnlock()
		return sub.mounts[vol.MountKey()]
	}())
	require.Error(t, err, "an unmount failure must still surface as an error")
	assert.True(t, detached,
		"a volume whose Detach succeeded is detached, even when Unmount failed")

	_, d, _, _ := drv.snapshot()
	assert.Equal(t, 1, d, "Detach must be attempted despite the unmount failure")
}

// fakeMountTable serves a canned mount table: stacks maps a path to the
// devices mounted there (bottom first), devs maps a device node path to
// its number. It is static: unmounting through the driver does not
// change it.
type fakeMountTable struct {
	stacks map[string][]mountinfo.Dev
	devs   map[string]mountinfo.Dev
}

func (f fakeMountTable) Stack(path string) ([]mountinfo.Dev, error) { return f.stacks[path], nil }
func (f fakeMountTable) MountPoints(d mountinfo.Dev) ([]string, error) {
	var out []string
	for p, stack := range f.stacks {
		for _, x := range stack {
			if x == d {
				out = append(out, p)
			}
		}
	}
	return out, nil
}
func (f fakeMountTable) DeviceOf(dev string) (mountinfo.Dev, error) {
	d, ok := f.devs[dev]
	if !ok {
		return mountinfo.Dev{}, errors.New("no such device: " + dev)
	}
	return d, nil
}

func mountTargets(drv *fakeDriver) map[driver.VolumeHandle]driver.MountTarget {
	drv.mu.Lock()
	defer drv.mu.Unlock()
	out := map[driver.VolumeHandle]driver.MountTarget{}
	for _, m := range drv.mountCalls {
		out[m.Handle] = m.Target
	}
	return out
}

// The same claim-template volume in two namespaces has one ID. Each must
// get its own mount path and its own tracking entry.
func TestSubsystem_SameIDInTwoNamespaces(t *testing.T) {
	sub, _, drv := newSubsystem(t, "node-a")
	ctx := context.Background()
	prod := boundVolume("mongodb-data-mongodb-0", "prod", "mongodb-data-mongodb-0", "node-a")
	prod.Handle = "rune-prod-mongodb-data-mongodb-0"
	trial := boundVolume("mongodb-data-mongodb-0", "trial", "mongodb-data-mongodb-0", "node-a")
	trial.Handle = "rune-trial-mongodb-data-mongodb-0"

	require.NoError(t, sub.reconcile(ctx, prod))
	require.NoError(t, sub.reconcile(ctx, trial))

	root := sub.cfg.MountRoot
	pt, ok := sub.MountTargetFor(prod.MountKey())
	require.True(t, ok)
	tt, ok := sub.MountTargetFor(trial.MountKey())
	require.True(t, ok)
	assert.Equal(t, filepath.Join(root, "prod", prod.ID), pt)
	assert.Equal(t, filepath.Join(root, "trial", trial.ID), tt)
	assert.Equal(t, map[driver.VolumeHandle]driver.MountTarget{
		"rune-prod-mongodb-data-mongodb-0":  driver.MountTarget(pt),
		"rune-trial-mongodb-data-mongodb-0": driver.MountTarget(tt),
	}, mountTargets(drv))
}

// A node upgraded from the namespace-blind layout has both namespaces'
// disks stacked on one old path, trial's mounted over prod's. Only the
// volume whose disk is visible there may keep the path; the other gets a
// fresh mount at its namespaced path rather than the other's data.
func TestSubsystem_LegacyPathAdoptedOnlyByVisibleDisk(t *testing.T) {
	sub, _, drv := newSubsystem(t, "node-a")
	ctx := context.Background()
	const id = "mongodb-data-mongodb-0"
	prod := boundVolume(id, "prod", id, "node-a")
	prod.Handle = "prod-disk"
	trial := boundVolume(id, "trial", id, "node-a")
	trial.Handle = "trial-disk"

	root := sub.cfg.MountRoot
	legacy := filepath.Join(root, id)
	prodDev, trialDev := mountinfo.Dev{Major: 8, Minor: 16}, mountinfo.Dev{Major: 8, Minor: 32}
	sub.cfg.MountTable = fakeMountTable{
		stacks: map[string][]mountinfo.Dev{legacy: {prodDev, trialDev}},
		devs:   map[string]mountinfo.Dev{"/dev/prod-disk": prodDev, "/dev/trial-disk": trialDev},
	}

	require.NoError(t, sub.reconcile(ctx, prod))
	require.NoError(t, sub.reconcile(ctx, trial))

	got := mountTargets(drv)
	assert.Equal(t, driver.MountTarget(filepath.Join(root, "prod", id)), got["prod-disk"],
		"prod's disk is buried under trial's; prod must not be handed the old path")
	assert.Equal(t, driver.MountTarget(legacy), got["trial-disk"],
		"trial's disk is the one visible at the old path, so trial keeps it")

	// prod's disk is now mounted twice. Tearing down the namespaced
	// mount must not detach a disk the buried mount still holds.
	sub.stateMu.Lock()
	m := sub.mounts[prod.MountKey()]
	delete(sub.mounts, prod.MountKey())
	sub.stateMu.Unlock()
	detached, err := sub.tearDown(ctx, prod.MountKey(), m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still mounted at "+legacy)
	assert.False(t, detached)
	_, d, _, u := drv.snapshot()
	assert.Equal(t, 1, u, "the namespaced mount is still unmounted")
	assert.Zero(t, d, "a disk still mounted at the old path must not be detached")
}

// After a crash with no drain, a disk mounted at its namespaced path
// stays there on restart, even if its old path now shows only that disk.
func TestSubsystem_NamespacedPathWinsOverLegacy(t *testing.T) {
	sub, _, drv := newSubsystem(t, "node-a")
	vol := boundVolume("data-db-0", "prod", "data-db-0", "node-a")
	root := sub.cfg.MountRoot
	dev := mountinfo.Dev{Major: 8, Minor: 16}
	sub.cfg.MountTable = fakeMountTable{
		stacks: map[string][]mountinfo.Dev{
			filepath.Join(root, vol.ID):         {dev},
			filepath.Join(root, "prod", vol.ID): {dev},
		},
		devs: map[string]mountinfo.Dev{"/dev/" + vol.Handle: dev},
	}
	require.NoError(t, sub.reconcile(context.Background(), vol))
	assert.Equal(t, driver.MountTarget(filepath.Join(root, "prod", vol.ID)),
		mountTargets(drv)[driver.VolumeHandle(vol.Handle)])
}

// A failed Mount must not detach a disk that is mounted somewhere else.
func TestSubsystem_NoDetachAfterFailedMountWhileMountedElsewhere(t *testing.T) {
	sub, _, drv := newSubsystem(t, "node-a")
	vol := boundVolume("data-db-0", "trial", "data-db-0", "node-a")
	dev := mountinfo.Dev{Major: 8, Minor: 16}
	sub.cfg.MountTable = fakeMountTable{
		stacks: map[string][]mountinfo.Dev{"/somewhere/else": {dev}},
		devs:   map[string]mountinfo.Dev{"/dev/" + vol.Handle: dev},
	}
	drv.mountErr = errors.New("mount failed")
	require.Error(t, sub.reconcile(context.Background(), vol))
	_, d, _, _ := drv.snapshot()
	assert.Zero(t, d)
}

// A volume alone on its old path keeps it, so running containers bound
// there keep working across the upgrade.
func TestSubsystem_LegacyPathAdopted(t *testing.T) {
	sub, _, drv := newSubsystem(t, "node-a")
	vol := boundVolume("data-db-0", "prod", "data-db-0", "node-a")
	legacy := filepath.Join(sub.cfg.MountRoot, vol.ID)
	dev := mountinfo.Dev{Major: 8, Minor: 16}
	sub.cfg.MountTable = fakeMountTable{
		stacks: map[string][]mountinfo.Dev{legacy: {dev}},
		devs:   map[string]mountinfo.Dev{"/dev/" + vol.Handle: dev},
	}
	require.NoError(t, sub.reconcile(context.Background(), vol))
	assert.Equal(t, driver.MountTarget(legacy), mountTargets(drv)[driver.VolumeHandle(vol.Handle)])
}

// Another namespace's disk on the old path is not ours to take.
func TestSubsystem_LegacyPathHeldByOtherDisk(t *testing.T) {
	sub, _, drv := newSubsystem(t, "node-a")
	vol := boundVolume("data-db-0", "trial", "data-db-0", "node-a")
	legacy := filepath.Join(sub.cfg.MountRoot, vol.ID)
	sub.cfg.MountTable = fakeMountTable{
		stacks: map[string][]mountinfo.Dev{legacy: {{Major: 8, Minor: 16}}},
		devs:   map[string]mountinfo.Dev{"/dev/" + vol.Handle: {Major: 8, Minor: 32}},
	}
	require.NoError(t, sub.reconcile(context.Background(), vol))
	assert.Equal(t, driver.MountTarget(filepath.Join(sub.cfg.MountRoot, "trial", vol.ID)),
		mountTargets(drv)[driver.VolumeHandle(vol.Handle)])
}

// An old-layout volume whose ID matches a namespace name occupies that
// namespace's directory. Mounting under it would write into that disk, so
// bring-up refuses. The disk is detached only if it is mounted nowhere.
func TestSubsystem_RefusesTargetInsideOldMount(t *testing.T) {
	for _, mountedElsewhere := range []bool{false, true} {
		sub, _, drv := newSubsystem(t, "node-a")
		vol := boundVolume("data-db-0", "prod", "data-db-0", "node-a")
		dev := mountinfo.Dev{Major: 8, Minor: 16}
		stacks := map[string][]mountinfo.Dev{filepath.Join(sub.cfg.MountRoot, "prod"): {{Major: 8, Minor: 48}}}
		if mountedElsewhere {
			stacks["/elsewhere"] = []mountinfo.Dev{dev}
		}
		sub.cfg.MountTable = fakeMountTable{stacks: stacks, devs: map[string]mountinfo.Dev{"/dev/" + vol.Handle: dev}}
		err := sub.reconcile(context.Background(), vol)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is a mount point")
		_, d, m, _ := drv.snapshot()
		assert.Zero(t, m)
		if mountedElsewhere {
			assert.Zero(t, d, "a disk mounted elsewhere stays attached")
		} else {
			assert.Equal(t, 1, d, "a disk mounted nowhere is not left attached")
		}
		_, ok := sub.MountTargetFor(vol.MountKey())
		assert.False(t, ok)
	}
}

// A disk whose teardown was refused because it is still mounted is not
// forgotten: once the disk above it is unmounted, the retry tick unmounts
// its leftover old-path mount and detaches it.
func TestSubsystem_ParkedDiskDetachedOnRetry(t *testing.T) {
	sub, _, drv := newSubsystem(t, "node-a")
	ctx := context.Background()
	const id = "mongodb-data-mongodb-0"
	prod := boundVolume(id, "prod", id, "node-a")
	root := sub.cfg.MountRoot
	legacy := filepath.Join(root, id)
	prodDev, trialDev := mountinfo.Dev{Major: 8, Minor: 16}, mountinfo.Dev{Major: 8, Minor: 32}
	table := fakeMountTable{
		stacks: map[string][]mountinfo.Dev{legacy: {prodDev, trialDev}},
		devs:   map[string]mountinfo.Dev{"/dev/" + prod.Handle: prodDev},
	}
	sub.cfg.MountTable = table
	require.NoError(t, sub.reconcile(ctx, prod))

	prod.BoundNode = "node-b" // unbound from this node
	require.Error(t, sub.reconcile(ctx, prod))
	_, d, _, _ := drv.snapshot()
	require.Zero(t, d)

	_, _, _, u0 := drv.snapshot()
	sub.retryParked(ctx) // still buried: nothing happens, not even an unmount
	_, d, _, u := drv.snapshot()
	require.Zero(t, d)
	require.Equal(t, u0, u, "a buried disk is not retried at all")

	// trial's disk goes; prod's is now the visible mount at the old path.
	table.stacks[legacy] = []mountinfo.Dev{prodDev}
	sub.retryParked(ctx)
	drv.mu.Lock()
	unmounted := append([]driver.MountTarget(nil), drv.unmountCalls...)
	drv.mu.Unlock()
	assert.Contains(t, unmounted, driver.MountTarget(legacy), "the leftover old-path mount is unmounted")
	// The static fake still lists the old path, so the detach is still
	// refused here; on a real host the unmount clears it.
	delete(table.stacks, legacy)
	sub.retryParked(ctx)
	_, d, _, _ = drv.snapshot()
	assert.Equal(t, 1, d)
	sub.stateMu.RLock()
	assert.Empty(t, sub.parked)
	sub.stateMu.RUnlock()
}

// The handle changed and the new disk now sits on the old disk's path.
// Retrying the old disk's teardown must not unmount the new one.
func TestSubsystem_ParkedRetryLeavesNewDiskOnSharedPath(t *testing.T) {
	sub, _, drv := newSubsystem(t, "node-a")
	ctx := context.Background()
	vol := boundVolume("data-db-0", "prod", "data-db-0", "node-a")
	vol.Handle = "old"
	target := filepath.Join(sub.cfg.MountRoot, "prod", vol.ID)
	oldDev, newDev := mountinfo.Dev{Major: 8, Minor: 16}, mountinfo.Dev{Major: 8, Minor: 32}
	table := fakeMountTable{
		stacks: map[string][]mountinfo.Dev{},
		devs:   map[string]mountinfo.Dev{"/dev/old": oldDev, "/dev/new": newDev},
	}
	sub.cfg.MountTable = table
	require.NoError(t, sub.reconcile(ctx, vol))

	// The old disk is also mounted outside the mount root, so its
	// detach is refused and it is parked.
	table.stacks["/elsewhere"] = []mountinfo.Dev{oldDev}
	vol.Handle = "new"
	require.NoError(t, sub.reconcile(ctx, vol))
	got, ok := sub.MountTargetFor(vol.MountKey())
	require.True(t, ok)
	require.Equal(t, target, got)
	table.stacks[target] = []mountinfo.Dev{newDev}

	_, _, _, u0 := drv.snapshot()
	sub.retryParked(ctx)
	drv.mu.Lock()
	later := append([]driver.MountTarget(nil), drv.unmountCalls[u0:]...)
	drv.mu.Unlock()
	assert.NotContains(t, later, driver.MountTarget(target), "the new disk's mount must not be taken down")
	_, d, _, _ := drv.snapshot()
	assert.Zero(t, d)
}

// A disk parked when runed stops is retried by the next run.
func TestSubsystem_ParkedDiskSurvivesRestart(t *testing.T) {
	sub, ts, drv := newSubsystem(t, "node-a")
	ctx := context.Background()
	vol := boundVolume("data-db-0", "prod", "data-db-0", "node-a")
	dev := mountinfo.Dev{Major: 8, Minor: 16}
	table := fakeMountTable{
		stacks: map[string][]mountinfo.Dev{"/elsewhere": {dev}},
		devs:   map[string]mountinfo.Dev{"/dev/" + vol.Handle: dev},
	}
	sub.cfg.MountTable = table
	require.NoError(t, sub.reconcile(ctx, vol))
	vol.BoundNode = "node-b"
	require.Error(t, sub.reconcile(ctx, vol))
	cause, ok := sub.MountErrorFor(vol.MountKey())
	require.True(t, ok)
	assert.Contains(t, cause, "still attached")
	_, err := os.Stat(filepath.Join(sub.cfg.MountRoot, parkedFile))
	require.NoError(t, err, "the parked disk is recorded on disk")

	// A new run over the same mount root, with the disk now free.
	next, err := New(Config{
		Store:      ts,
		NodeID:     "node-a",
		Lookup:     func(context.Context, *types.Volume) (driver.Driver, error) { return drv, nil },
		MountRoot:  sub.cfg.MountRoot,
		MountTable: fakeMountTable{devs: table.devs},
		Logger:     log.NewLogger(),
	})
	require.NoError(t, err)
	require.NoError(t, next.Start(ctx))
	t.Cleanup(func() { _ = next.Stop(context.Background()) })
	next.retryParked(ctx)
	_, d, _, _ := drv.snapshot()
	assert.Equal(t, 1, d, "the next run detaches it")
	_, err = os.Stat(filepath.Join(sub.cfg.MountRoot, parkedFile))
	assert.True(t, os.IsNotExist(err), "and forgets it")
}

// A parked disk that comes back into use is not torn down.
func TestSubsystem_ParkedDiskInUseAgainIsKept(t *testing.T) {
	sub, _, drv := newSubsystem(t, "node-a")
	ctx := context.Background()
	vol := boundVolume("data-db-0", "prod", "data-db-0", "node-a")
	dev := mountinfo.Dev{Major: 8, Minor: 16}
	sub.cfg.MountTable = fakeMountTable{
		stacks: map[string][]mountinfo.Dev{"/elsewhere": {dev}},
		devs:   map[string]mountinfo.Dev{"/dev/" + vol.Handle: dev},
	}
	require.NoError(t, sub.reconcile(ctx, vol))
	vol.BoundNode = "node-b"
	require.Error(t, sub.reconcile(ctx, vol))
	vol.BoundNode = "node-a"
	require.NoError(t, sub.reconcile(ctx, vol))
	sub.cfg.MountTable = fakeMountTable{devs: map[string]mountinfo.Dev{"/dev/" + vol.Handle: dev}}
	sub.retryParked(ctx)
	_, d, _, _ := drv.snapshot()
	assert.Zero(t, d)
	_, ok := sub.MountTargetFor(vol.MountKey())
	assert.True(t, ok)
}

// IDs and namespaces that would walk out of the mount root are refused
// before anything is mounted.
func TestSubsystem_RefusesUnsafeMountKeys(t *testing.T) {
	for _, id := range []string{"../prod/data-api-0", "a/b", "..", "."} {
		sub, _, drv := newSubsystem(t, "node-a")
		vol := boundVolume(id, "trial", "data", "node-a")
		err := sub.reconcile(context.Background(), vol)
		require.Error(t, err, id)
		assert.Contains(t, err.Error(), "unsafe mount key", id)
		_, _, m, _ := drv.snapshot()
		assert.Zero(t, m, id)
	}
}

// Another disk on the target path is refused: the driver would see "a
// mount is there" and report success with the wrong disk.
func TestSubsystem_RefusesForeignDiskAtTarget(t *testing.T) {
	sub, _, drv := newSubsystem(t, "node-a")
	vol := boundVolume("data-db-0", "trial", "data-db-0", "node-a")
	sub.cfg.MountTable = fakeMountTable{
		stacks: map[string][]mountinfo.Dev{filepath.Join(sub.cfg.MountRoot, "trial", vol.ID): {{Major: 8, Minor: 16}}},
		devs:   map[string]mountinfo.Dev{"/dev/" + vol.Handle: {Major: 8, Minor: 32}},
	}
	err := sub.reconcile(context.Background(), vol)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another disk")
	_, _, m, _ := drv.snapshot()
	assert.Zero(t, m)
}
