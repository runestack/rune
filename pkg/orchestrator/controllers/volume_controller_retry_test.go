package controllers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retryDriverNameCounter generates unique driver names per test so we
// don't clash with the global driver registry across the file.
var retryDriverNameCounter atomic.Uint64

func uniqueRetryDriverName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, retryDriverNameCounter.Add(1))
}

// retryTestDriver is a Driver stub for the controller's retry/backoff path.
// It fails the first failUntil Provision calls (with errProvision) and
// then succeeds, returning successHandle. failUntil < 0 means
// "always fail".
type retryTestDriver struct {
	name          string
	failUntil     int
	successHandle string

	mu    sync.Mutex
	calls int
}

func (d *retryTestDriver) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *retryTestDriver) Name() string { return d.name }
func (d *retryTestDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{AccessModes: []types.AccessMode{types.AccessModeRWO}}
}
func (d *retryTestDriver) Provision(_ context.Context, _ driver.OpContext, _ driver.ProvisionRequest) (driver.VolumeHandle, error) {
	d.mu.Lock()
	d.calls++
	n := d.calls
	d.mu.Unlock()
	if d.failUntil < 0 || n <= d.failUntil {
		return "", errors.New("simulated provision failure")
	}
	return driver.VolumeHandle(d.successHandle), nil
}
func (d *retryTestDriver) Delete(context.Context, driver.OpContext, driver.VolumeHandle) error {
	return nil
}
func (d *retryTestDriver) Attach(context.Context, driver.OpContext, driver.VolumeHandle, driver.NodeID) (driver.DevicePath, error) {
	return "", nil
}
func (d *retryTestDriver) Detach(context.Context, driver.OpContext, driver.VolumeHandle, driver.NodeID) error {
	return nil
}
func (d *retryTestDriver) Mount(context.Context, driver.OpContext, driver.MountOpts) (driver.MountTarget, error) {
	return "", nil
}
func (d *retryTestDriver) Unmount(context.Context, driver.OpContext, driver.MountTarget) error {
	return nil
}
func (d *retryTestDriver) Snapshot(context.Context, driver.OpContext, driver.SnapshotRequest) (driver.SnapshotHandle, error) {
	return "", driver.ErrUnsupported
}
func (d *retryTestDriver) RestoreFromSnapshot(context.Context, driver.OpContext, driver.RestoreRequest) (driver.VolumeHandle, error) {
	return "", driver.ErrUnsupported
}
func (d *retryTestDriver) DeleteSnapshot(context.Context, driver.OpContext, driver.SnapshotHandle) error {
	return driver.ErrUnsupported
}
func (d *retryTestDriver) Expand(context.Context, driver.OpContext, driver.VolumeHandle, string) error {
	return driver.ErrUnsupported
}

// registerRetryDriver registers a fresh retryTestDriver under a unique
// name and returns the name + a handle to the driver instance the
// controller will receive (the registry caches the factory result, so
// the controller and the test see the same struct).
func registerRetryDriver(t *testing.T, prefix string, failUntil int) (string, *retryTestDriver) {
	t.Helper()
	name := uniqueRetryDriverName(prefix)
	d := &retryTestDriver{
		name:          name,
		failUntil:     failUntil,
		successHandle: "/tmp/" + name,
	}
	driver.Register(name, func(map[string]any) (driver.Driver, error) { return d, nil })
	return name, d
}

// setupVolumeControllerWithOptions is like setupVolumeController but lets
// the test override retry knobs and driver wiring.
func setupVolumeControllerWithOptions(t *testing.T, opts VolumeControllerOptions) (context.Context, context.CancelFunc, *store.TestStore, VolumeController) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	testStore := store.NewTestStore()
	opts.Store = testStore
	if opts.Logger == nil {
		opts.Logger = log.NewLogger()
	}

	controller, err := NewVolumeController(opts)
	require.NoError(t, err)
	require.NoError(t, controller.Start(ctx))

	t.Cleanup(func() {
		_ = controller.Stop()
		cancel()
	})
	return ctx, cancel, testStore, controller
}

func TestVolumeController_RetryThenStalled(t *testing.T) {
	driverName, drv := registerRetryDriver(t, "retry-stalled", -1) // always fail

	ctx, _, ts, _ := setupVolumeControllerWithOptions(t, VolumeControllerOptions{
		MaxProvisionAttempts: 3,
		ProvisionBaseBackoff: 5 * time.Millisecond,
		ProvisionMaxBackoff:  10 * time.Millisecond,
	})
	putStorageClass(t, ts, "sc-stalled", driverName)

	vol := &types.Volume{
		ID:               "v-stalled",
		Name:             "stuck",
		Namespace:        "default",
		StorageClassName: "sc-stalled",
		AccessMode:       types.AccessModeRWO,
		Status:           types.VolumeStatusPending,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 3*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusStalled {
			return fmt.Errorf("not yet stalled: %s (reason=%q)", got.Status, got.Reason)
		}
		return nil
	})

	got := loadVolume(t, ts, vol.Namespace, vol.Name)
	assert.Equal(t, "ProvisionRetriesExhausted", got.Reason)
	assert.NotEmpty(t, got.Message)
	assert.Empty(t, got.Handle, "no handle when stalled")
	assert.GreaterOrEqual(t, drv.callCount(), 3, "all attempts exhausted")
}

func TestVolumeController_RetrySucceedsAfterTransientFailure(t *testing.T) {
	driverName, drv := registerRetryDriver(t, "retry-success", 2) // fail twice, succeed on third

	ctx, _, ts, _ := setupVolumeControllerWithOptions(t, VolumeControllerOptions{
		MaxProvisionAttempts: 5,
		ProvisionBaseBackoff: 5 * time.Millisecond,
		ProvisionMaxBackoff:  10 * time.Millisecond,
	})
	putStorageClass(t, ts, "sc-recover", driverName)

	vol := &types.Volume{
		ID:               "v-recover",
		Name:             "blip",
		Namespace:        "default",
		StorageClassName: "sc-recover",
		AccessMode:       types.AccessModeRWO,
		Status:           types.VolumeStatusPending,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 3*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusAvailable {
			return fmt.Errorf("not yet available: %s (reason=%q)", got.Status, got.Reason)
		}
		return nil
	})

	got := loadVolume(t, ts, vol.Namespace, vol.Name)
	assert.NotEmpty(t, got.Handle)
	assert.Empty(t, got.Reason)
	assert.Empty(t, got.Message)
	assert.Equal(t, 3, drv.callCount(), "fail-fail-success")
}
