package controllers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver/local"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupVolumeController wires a controller against an in-memory store and
// the in-tree "local" driver pointed at a temp directory. Each test gets
// its own driver root so we don't trip the global driver cache between
// tests sharing the same controller.
func setupVolumeController(t *testing.T) (context.Context, context.CancelFunc, *store.TestStore, VolumeController, string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	testStore := store.NewTestStore()

	root := t.TempDir()
	controller, err := NewVolumeController(VolumeControllerOptions{
		Store:  testStore,
		Logger: log.NewLogger(),
		DriverConfigs: map[string]map[string]any{
			local.DriverNameLocal: {
				"localVolumeRoot": root,
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, controller.Start(ctx))

	t.Cleanup(func() {
		_ = controller.Stop()
		cancel()
	})
	return ctx, cancel, testStore, controller, root
}

// putStorageClass seeds a StorageClass row directly via Create.
func putStorageClass(t *testing.T, s *store.TestStore, name, driverName string) {
	t.Helper()
	class := &types.StorageClass{
		ID:        "sc-" + name,
		Name:      name,
		Driver:    driverName,
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, s.Create(context.Background(), types.ResourceTypeStorageClass, "", name, class))
}

// waitFor polls fn until it returns nil or the deadline expires. Eventual
// status flips from the controller arrive on a goroutine, so the test
// must spin instead of asserting synchronously.
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

// loadVolume helpfully fetches the latest stored copy so tests can read
// status transitions back.
func loadVolume(t *testing.T, s *store.TestStore, ns, name string) *types.Volume {
	t.Helper()
	var v types.Volume
	require.NoError(t, s.Get(context.Background(), types.ResourceTypeVolume, ns, name, &v))
	return &v
}

func TestVolumeController_ProvisionsOnCreate(t *testing.T) {
	ctx, _, ts, _, root := setupVolumeController(t)
	putStorageClass(t, ts, "sc-1", local.DriverNameLocal)

	vol := &types.Volume{
		ID:               "v-1",
		Name:             "data",
		Namespace:        "default",
		StorageClassName: "sc-1",
		Size:             "1Mi",
		AccessMode:       types.AccessModeRWO,
		ReclaimPolicy:    types.ReclaimPolicyDelete,
		Status:           types.VolumeStatusPending,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 2*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusAvailable {
			return errors.New("not yet available: " + string(got.Status))
		}
		if got.Handle == "" {
			return errors.New("handle is empty")
		}
		return nil
	})

	got := loadVolume(t, ts, vol.Namespace, vol.Name)
	expectedDir := filepath.Join(root, vol.Namespace, vol.Name)
	assert.Equal(t, expectedDir, got.Handle, "handle should be the volume directory")
	if _, err := os.Stat(got.Handle); err != nil {
		t.Fatalf("handle directory was not created: %v", err)
	}
}

func TestVolumeController_FailsOnMissingStorageClass(t *testing.T) {
	ctx, _, ts, _, _ := setupVolumeController(t)

	vol := &types.Volume{
		ID:               "v-2",
		Name:             "orphan",
		Namespace:        "default",
		StorageClassName: "no-such-class",
		AccessMode:       types.AccessModeRWO,
		Status:           types.VolumeStatusPending,
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 2*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusFailed {
			return errors.New("not yet failed: " + string(got.Status))
		}
		return nil
	})

	got := loadVolume(t, ts, vol.Namespace, vol.Name)
	assert.Equal(t, "StorageClassMissing", got.Reason)
	assert.NotEmpty(t, got.Message)
	assert.Empty(t, got.Handle, "no handle when class lookup failed")
}

func TestVolumeController_ReclaimsOnDeleteWithDeletePolicy(t *testing.T) {
	ctx, _, ts, _, root := setupVolumeController(t)
	putStorageClass(t, ts, "sc-2", local.DriverNameLocal)

	vol := &types.Volume{
		ID:               "v-3",
		Name:             "ephemeral",
		Namespace:        "default",
		StorageClassName: "sc-2",
		AccessMode:       types.AccessModeRWO,
		ReclaimPolicy:    types.ReclaimPolicyDelete,
		Status:           types.VolumeStatusPending,
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	// Wait for provision.
	waitFor(t, 2*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusAvailable {
			return errors.New("not yet available")
		}
		return nil
	})
	provisioned := loadVolume(t, ts, vol.Namespace, vol.Name)
	require.NotEmpty(t, provisioned.Handle)

	// Delete and expect the directory to disappear.
	require.NoError(t, ts.Delete(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name))
	waitFor(t, 2*time.Second, func() error {
		if _, err := os.Stat(provisioned.Handle); err == nil {
			return errors.New("handle directory still present")
		}
		return nil
	})
	// And the handle must have been inside the configured root — sanity.
	assert.Contains(t, provisioned.Handle, root)
}

func TestVolumeController_RetainsOnDeleteWithRetainPolicy(t *testing.T) {
	ctx, _, ts, _, _ := setupVolumeController(t)
	putStorageClass(t, ts, "sc-3", local.DriverNameLocal)

	vol := &types.Volume{
		ID:               "v-4",
		Name:             "keepme",
		Namespace:        "default",
		StorageClassName: "sc-3",
		AccessMode:       types.AccessModeRWO,
		ReclaimPolicy:    types.ReclaimPolicyRetain,
		Status:           types.VolumeStatusPending,
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 2*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusAvailable {
			return errors.New("not yet available")
		}
		return nil
	})
	provisioned := loadVolume(t, ts, vol.Namespace, vol.Name)

	require.NoError(t, ts.Delete(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name))

	// Give the controller a beat to (not) do anything.
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(provisioned.Handle); err != nil {
		t.Fatalf("retain policy must leave handle in place, got: %v", err)
	}
}

// TestVolumeController_SeedsBuiltInStorageClasses verifies that Start
// idempotently creates the built-in "local" (Default:true) and "local-host"
// classes. setupVolumeController already calls Start, so we just inspect
// the store.
func TestVolumeController_SeedsBuiltInStorageClasses(t *testing.T) {
	_, _, ts, _, _ := setupVolumeController(t)

	var localSC types.StorageClass
	require.NoError(t, ts.Get(context.Background(), types.ResourceTypeStorageClass, "", "local", &localSC))
	assert.Equal(t, "local", localSC.Driver)
	assert.True(t, localSC.Default, "built-in 'local' class must be Default:true")
	assert.Equal(t, types.ReclaimPolicyRetain, localSC.ReclaimPolicy)
	assert.Equal(t, "true", localSC.Labels["rune.io/builtin"])

	var hostSC types.StorageClass
	require.NoError(t, ts.Get(context.Background(), types.ResourceTypeStorageClass, "", "local-host", &hostSC))
	assert.Equal(t, "local-host", hostSC.Driver)
	assert.False(t, hostSC.Default, "built-in 'local-host' must not be Default")
}

// TestVolumeController_SeedRespectsExistingClass — if an operator (or a
// previous boot) has already created a class with the same name, the seed
// step must not clobber it.
func TestVolumeController_SeedRespectsExistingClass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ts := store.NewTestStore()

	// Pre-create a customised "local" class with Default:false. Seeding
	// must leave it alone.
	preExisting := &types.StorageClass{
		ID:            "user-local",
		Name:          "local",
		Driver:        "local",
		ReclaimPolicy: types.ReclaimPolicyDelete,
		Default:       false,
		Labels:        map[string]string{"operator": "owned"},
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeStorageClass, "", "local", preExisting))

	controller, err := NewVolumeController(VolumeControllerOptions{
		Store:  ts,
		Logger: log.NewLogger(),
	})
	require.NoError(t, err)
	require.NoError(t, controller.Start(ctx))
	t.Cleanup(func() { _ = controller.Stop() })

	var got types.StorageClass
	require.NoError(t, ts.Get(ctx, types.ResourceTypeStorageClass, "", "local", &got))
	assert.Equal(t, "user-local", got.ID, "operator-owned class must not be overwritten")
	assert.False(t, got.Default, "operator-set Default:false must be preserved")
	assert.Equal(t, "owned", got.Labels["operator"])
}

// TestVolumeController_ResolvesDefaultClassWhenUnspecified — a Volume with
// an empty StorageClassName must provision against the seeded Default class.
func TestVolumeController_ResolvesDefaultClassWhenUnspecified(t *testing.T) {
	ctx, _, ts, _, _ := setupVolumeController(t)

	vol := &types.Volume{
		ID:            "v-default",
		Name:          "no-class-specified",
		Namespace:     "default",
		AccessMode:    types.AccessModeRWO,
		ReclaimPolicy: types.ReclaimPolicyRetain,
		Status:        types.VolumeStatusPending,
		// StorageClassName intentionally empty — should resolve to seeded
		// "local" class which is Default:true.
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	waitFor(t, 2*time.Second, func() error {
		got := loadVolume(t, ts, vol.Namespace, vol.Name)
		if got.Status != types.VolumeStatusAvailable {
			return errors.New("not yet available: " + string(got.Status))
		}
		if got.Handle == "" {
			return errors.New("handle empty")
		}
		return nil
	})
}
