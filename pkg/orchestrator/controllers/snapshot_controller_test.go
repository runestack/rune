package controllers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver/local"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/require"
)

// setupSnapshotController wires a SnapshotController against an in-memory
// store and the in-tree "local" driver pointed at a temp directory.
func setupSnapshotController(t *testing.T) (context.Context, *store.TestStore, SnapshotController, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ts := store.NewTestStore()
	root := t.TempDir()
	snapRoot := filepath.Join(root, ".snapshots")
	ctrl, err := NewSnapshotController(SnapshotControllerOptions{
		Store:  ts,
		Logger: log.NewLogger(),
		DriverConfigs: map[string]map[string]any{
			local.DriverNameLocal: {
				"localVolumeRoot": root,
				"snapshotRoot":    snapRoot,
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, ctrl.Start(ctx))
	t.Cleanup(func() {
		_ = ctrl.Stop()
		cancel()
	})
	return ctx, ts, ctrl, snapRoot
}

func loadSnapshot(t *testing.T, s *store.TestStore, ns, name string) *types.Snapshot {
	t.Helper()
	var snap types.Snapshot
	require.NoError(t, s.Get(context.Background(), types.ResourceTypeSnapshot, ns, name, &snap))
	return &snap
}

func snapshotMissing(t *testing.T, s *store.TestStore, ns, name string) error {
	t.Helper()
	var snap types.Snapshot
	err := s.Get(context.Background(), types.ResourceTypeSnapshot, ns, name, &snap)
	if err == nil {
		return errors.New("snapshot still exists")
	}
	return nil
}

// seedProvisionedVolume writes a Volume row already in Available state
// with a real on-disk handle so the snapshot driver can copytree it.
func seedProvisionedVolume(t *testing.T, ts *store.TestStore, ns, name, scName, root string) *types.Volume {
	t.Helper()
	dir := filepath.Join(root, ns, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data.txt"), []byte("hello"), 0o644))
	vol := &types.Volume{
		ID:               "vol-" + name,
		Name:             name,
		Namespace:        ns,
		StorageClassName: scName,
		Size:             "1Mi",
		AccessMode:       types.AccessModeRWO,
		ReclaimPolicy:    types.ReclaimPolicyDelete,
		Status:           types.VolumeStatusAvailable,
		Handle:           dir,
		CreatedAt:        time.Now().UTC(),
	}
	require.NoError(t, ts.Create(context.Background(), types.ResourceTypeVolume, ns, name, vol))
	return vol
}

func TestSnapshotController_PendingToReady(t *testing.T) {
	ctx, ts, _, _ := setupSnapshotController(t)
	putStorageClass(t, ts, "sc-snap", local.DriverNameLocal)
	seedProvisionedVolume(t, ts, "default", "data", "sc-snap", t.TempDir())

	snap := &types.Snapshot{
		ID:           "snap-1",
		Name:         "snap1",
		Namespace:    "default",
		SourceVolume: "data",
		Phase:        types.SnapshotPhasePending,
		CreatedAt:    time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeSnapshot, snap.Namespace, snap.Name, snap))

	waitFor(t, 3*time.Second, func() error {
		got := loadSnapshot(t, ts, snap.Namespace, snap.Name)
		if got.Phase != types.SnapshotPhaseReady {
			return errors.New("not ready: " + string(got.Phase))
		}
		if got.Handle == "" {
			return errors.New("handle empty")
		}
		return nil
	})
}

func TestSnapshotController_FailsOnMissingSourceVolume(t *testing.T) {
	ctx, ts, _, _ := setupSnapshotController(t)
	putStorageClass(t, ts, "sc-snap", local.DriverNameLocal)

	snap := &types.Snapshot{
		ID:           "snap-2",
		Name:         "snap2",
		Namespace:    "default",
		SourceVolume: "ghost",
		Phase:        types.SnapshotPhasePending,
		CreatedAt:    time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeSnapshot, snap.Namespace, snap.Name, snap))

	waitFor(t, 2*time.Second, func() error {
		got := loadSnapshot(t, ts, snap.Namespace, snap.Name)
		if got.Phase != types.SnapshotPhaseFailed {
			return errors.New("not failed: " + string(got.Phase))
		}
		if got.Reason != "SourceVolumeMissing" {
			return errors.New("reason: " + got.Reason)
		}
		return nil
	})
}

func TestSnapshotController_DeletingDrivesDriverThenRemovesRow(t *testing.T) {
	ctx, ts, _, snapRoot := setupSnapshotController(t)
	putStorageClass(t, ts, "sc-snap", local.DriverNameLocal)
	seedProvisionedVolume(t, ts, "default", "data", "sc-snap", t.TempDir())

	snap := &types.Snapshot{
		ID:           "snap-3",
		Name:         "snap3",
		Namespace:    "default",
		SourceVolume: "data",
		Phase:        types.SnapshotPhasePending,
		CreatedAt:    time.Now().UTC(),
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeSnapshot, snap.Namespace, snap.Name, snap))

	// Wait for Ready + on-disk snapshot tree.
	var ready *types.Snapshot
	waitFor(t, 3*time.Second, func() error {
		got := loadSnapshot(t, ts, snap.Namespace, snap.Name)
		if got.Phase != types.SnapshotPhaseReady {
			return errors.New("not ready: " + string(got.Phase))
		}
		ready = got
		return nil
	})
	if _, err := os.Stat(ready.Handle); err != nil {
		t.Fatalf("snapshot dir missing: %v", err)
	}
	if rel, err := filepath.Rel(snapRoot, ready.Handle); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("snapshot handle %q not under root %q (rel=%q err=%v)", ready.Handle, snapRoot, rel, err)
	}

	// Flip to Deleting; controller must call DeleteSnapshot and remove the row.
	ready.Phase = types.SnapshotPhaseDeleting
	require.NoError(t, ts.Update(ctx, types.ResourceTypeSnapshot, ready.Namespace, ready.Name, ready))

	waitFor(t, 2*time.Second, func() error {
		return snapshotMissing(t, ts, snap.Namespace, snap.Name)
	})
	if _, err := os.Stat(ready.Handle); !os.IsNotExist(err) {
		t.Fatalf("expected snapshot dir to be removed, stat err=%v", err)
	}
}
