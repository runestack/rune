package service

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

func newSnapshotTestServices(t *testing.T) (*SnapshotService, *VolumeService, store.Store) {
	t.Helper()
	st := store.NewTestStoreWithOptions(store.StoreOptions{
		ConfigLimits: store.Limits{MaxObjectBytes: 1 << 20, MaxKeyNameLength: 256},
	})
	return NewSnapshotService(st, log.GetDefaultLogger()),
		NewVolumeService(st, log.GetDefaultLogger()),
		st
}

// seedVolume writes a Volume row directly through the repo so the
// snapshot service has a source to look up.
func seedVolume(t *testing.T, st store.Store, ns, name string) {
	t.Helper()
	repo := repos.NewVolumeRepo(st)
	v := &types.Volume{
		Name:             name,
		Namespace:        ns,
		StorageClassName: "local",
		Size:             "1Gi",
		AccessMode:       types.AccessModeRWO,
		Status:           types.VolumeStatusAvailable,
		Handle:           "h-" + name,
	}
	if err := repo.Create(context.Background(), v); err != nil {
		t.Fatalf("seed volume: %v", err)
	}
}

func TestSnapshotServiceCRUD(t *testing.T) {
	ctx := context.Background()
	svc, _, st := newSnapshotTestServices(t)
	seedVolume(t, st, "prod", "data")

	resp, err := svc.CreateSnapshot(ctx, &generated.CreateSnapshotRequest{
		Snapshot: &generated.Snapshot{
			Name:         "snap1",
			Namespace:    "prod",
			SourceVolume: "data",
		},
		EnsureNamespace: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.Snapshot.Phase != string(types.SnapshotPhasePending) {
		t.Fatalf("phase: want Pending, got %q", resp.Snapshot.Phase)
	}

	getResp, err := svc.GetSnapshot(ctx, &generated.GetSnapshotRequest{Name: "snap1", Namespace: "prod"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if getResp.Snapshot.SourceVolume != "data" {
		t.Fatalf("source: %s", getResp.Snapshot.SourceVolume)
	}

	listResp, err := svc.ListSnapshots(ctx, &generated.ListSnapshotsRequest{Namespace: "prod"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listResp.Snapshots) != 1 {
		t.Fatalf("list: want 1, got %d", len(listResp.Snapshots))
	}

	// Delete on a Pending snapshot (no Handle) hard-deletes the row.
	if _, err := svc.DeleteSnapshot(ctx, &generated.DeleteSnapshotRequest{Name: "snap1", Namespace: "prod"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.GetSnapshot(ctx, &generated.GetSnapshotRequest{Name: "snap1", Namespace: "prod"}); err == nil {
		t.Fatalf("expected NotFound after delete")
	}
}

func TestSnapshotServiceCreateMissingSource(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newSnapshotTestServices(t)
	_, err := svc.CreateSnapshot(ctx, &generated.CreateSnapshotRequest{
		Snapshot: &generated.Snapshot{
			Name:         "orphan",
			Namespace:    "prod",
			SourceVolume: "ghost",
		},
		EnsureNamespace: true,
	})
	if err == nil {
		t.Fatalf("expected FailedPrecondition for missing source volume")
	}
}

func TestSnapshotServiceDeleteTwoPhase(t *testing.T) {
	ctx := context.Background()
	svc, _, st := newSnapshotTestServices(t)
	seedVolume(t, st, "prod", "data")

	if _, err := svc.CreateSnapshot(ctx, &generated.CreateSnapshotRequest{
		Snapshot: &generated.Snapshot{
			Name:         "snap1",
			Namespace:    "prod",
			SourceVolume: "data",
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Promote to Ready with a Handle so Delete takes the two-phase path.
	repo := repos.NewSnapshotRepo(st)
	cur, err := repo.Get(ctx, "prod", "snap1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	cur.Phase = types.SnapshotPhaseReady
	cur.Handle = "/tmp/snap1"
	cur.Driver = "local"
	if err := repo.Update(ctx, cur); err != nil {
		t.Fatalf("promote: %v", err)
	}

	if _, err := svc.DeleteSnapshot(ctx, &generated.DeleteSnapshotRequest{Name: "snap1", Namespace: "prod"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Row must still exist, in phase Deleting (controller does the rest).
	got, err := repo.Get(ctx, "prod", "snap1")
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if got.Phase != types.SnapshotPhaseDeleting {
		t.Fatalf("phase: want Deleting, got %q", got.Phase)
	}
}

func TestSnapshotServiceRestoreVolume(t *testing.T) {
	ctx := context.Background()
	svc, vsvc, st := newSnapshotTestServices(t)
	seedVolume(t, st, "prod", "data")

	// Create + promote snapshot to Ready.
	if _, err := svc.CreateSnapshot(ctx, &generated.CreateSnapshotRequest{
		Snapshot: &generated.Snapshot{
			Name:         "snap1",
			Namespace:    "prod",
			SourceVolume: "data",
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("create snap: %v", err)
	}
	repo := repos.NewSnapshotRepo(st)
	cur, err := repo.Get(ctx, "prod", "snap1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	cur.Phase = types.SnapshotPhaseReady
	cur.Handle = "/tmp/snap1"
	cur.Driver = "local"
	if err := repo.Update(ctx, cur); err != nil {
		t.Fatalf("promote: %v", err)
	}

	resp, err := svc.RestoreVolume(ctx, &generated.RestoreVolumeRequest{
		SnapshotName:      "snap1",
		SnapshotNamespace: "prod",
		TargetVolumeName:  "data-restored",
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if resp.Volume.Name != "data-restored" {
		t.Fatalf("target name: %s", resp.Volume.Name)
	}
	// The restore stamp must be on the target volume.
	got, err := vsvc.GetVolume(ctx, &generated.GetVolumeRequest{Name: "data-restored", Namespace: "prod"})
	if err != nil {
		t.Fatalf("get target: %v", err)
	}
	stamp := got.Volume.Parameters[types.RestoreFromSnapshotParam]
	if stamp != "prod/snap1" {
		t.Fatalf("restore stamp: want %q, got %q", "prod/snap1", stamp)
	}
}

func TestSnapshotServiceRestoreNotReady(t *testing.T) {
	ctx := context.Background()
	svc, _, st := newSnapshotTestServices(t)
	seedVolume(t, st, "prod", "data")
	if _, err := svc.CreateSnapshot(ctx, &generated.CreateSnapshotRequest{
		Snapshot: &generated.Snapshot{
			Name:         "snap1",
			Namespace:    "prod",
			SourceVolume: "data",
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("create snap: %v", err)
	}
	// Snapshot is still Pending; restore must fail.
	if _, err := svc.RestoreVolume(ctx, &generated.RestoreVolumeRequest{
		SnapshotName:      "snap1",
		SnapshotNamespace: "prod",
		TargetVolumeName:  "data-restored",
	}); err == nil {
		t.Fatalf("expected restore to fail when snapshot not Ready")
	}
}
