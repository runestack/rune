package local_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/storage/driver/local"
	"github.com/runestack/rune/pkg/storage/drivertest"
	"github.com/runestack/rune/pkg/types"
)

// newManagedDriver builds the managed driver directly (bypassing the
// registry) so the test isn't coupled to package init() ordering.
func newManagedDriver(t *testing.T) (driver.Driver, string) {
	t.Helper()
	root := t.TempDir()
	d, err := driver.New(local.DriverNameLocal, map[string]any{
		"localVolumeRoot": root,
		"snapshotRoot":    filepath.Join(root, ".snapshots"),
	})
	if err != nil {
		t.Fatalf("New(local): %v", err)
	}
	return d, root
}

func newHostDriver(t *testing.T, allowlistRoot string) driver.Driver {
	t.Helper()
	d, err := driver.New(local.DriverNameLocalHost, map[string]any{
		"hostPathAllowlist":  []string{allowlistRoot},
		"allowCreateMissing": true,
	})
	if err != nil {
		t.Fatalf("New(local-host): %v", err)
	}
	return d
}

func TestLocalConformance(t *testing.T) {
	d, root := newManagedDriver(t)
	counter := 0
	drivertest.RunConformance(t, drivertest.Config{
		Driver: d,
		NewVolume: func(t *testing.T) (*types.Volume, *types.StorageClass, map[string]string) {
			counter++
			vol := &types.Volume{
				Name:             "vol-" + sprintfInt(counter),
				Namespace:        "default",
				StorageClassName: "local",
				Size:             "1Mi",
				AccessMode:       types.AccessModeRWO,
			}
			class := &types.StorageClass{Name: "local", Driver: local.DriverNameLocal}
			return vol, class, nil
		},
		MountTargetRoot: filepath.Join(root, ".mounts"),
	})
}

func TestLocalHostConformance(t *testing.T) {
	allowRoot := t.TempDir()
	d := newHostDriver(t, allowRoot)
	counter := 0
	drivertest.RunConformance(t, drivertest.Config{
		Driver: d,
		NewVolume: func(t *testing.T) (*types.Volume, *types.StorageClass, map[string]string) {
			counter++
			hostPath := filepath.Join(allowRoot, "vol-"+sprintfInt(counter))
			if err := os.MkdirAll(hostPath, 0o750); err != nil {
				t.Fatalf("mkdir host path: %v", err)
			}
			vol := &types.Volume{
				Name:             "host-" + sprintfInt(counter),
				Namespace:        "default",
				StorageClassName: "local-host",
				Size:             "1Mi",
				AccessMode:       types.AccessModeRWO,
				Parameters:       map[string]string{"hostPath": hostPath},
			}
			class := &types.StorageClass{Name: "local-host", Driver: local.DriverNameLocalHost}
			return vol, class, map[string]string{"hostPath": hostPath}
		},
	})
}

func TestLocalRefusesDeleteOutsideRoot(t *testing.T) {
	d, _ := newManagedDriver(t)
	stranger := t.TempDir() // not under the driver's localVolumeRoot
	target := filepath.Join(stranger, "should-not-be-touched")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatalf("setup: %v", err)
	}

	err := d.Delete(context.Background(), driver.OpContext{}, driver.VolumeHandle(target))
	if err == nil {
		t.Fatal("Delete(outside-root): want error, got nil")
	}
	if !errors.Is(err, driver.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatalf("target was deleted despite refusal: %v", statErr)
	}
}

func TestLocalPreserveOnDelete(t *testing.T) {
	root := t.TempDir()
	d, err := driver.New(local.DriverNameLocal, map[string]any{
		"localVolumeRoot":  root,
		"preserveOnDelete": true,
	})
	if err != nil {
		t.Fatalf("New(local): %v", err)
	}
	vol := &types.Volume{Name: "keep-me", Namespace: "default", AccessMode: types.AccessModeRWO}
	opctx := driver.OpContext{Volume: vol}
	handle, err := d.Provision(context.Background(), opctx, driver.ProvisionRequest{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := d.Delete(context.Background(), opctx, handle); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(string(handle)); err != nil {
		t.Fatalf("preserveOnDelete=true should keep directory, got: %v", err)
	}
}

func TestLocalHostRejectsReclaimDelete(t *testing.T) {
	allowRoot := t.TempDir()
	d := newHostDriver(t, allowRoot)
	hostPath := filepath.Join(allowRoot, "v")
	if err := os.MkdirAll(hostPath, 0o750); err != nil {
		t.Fatalf("setup: %v", err)
	}
	vol := &types.Volume{
		Name:          "v",
		Namespace:     "default",
		AccessMode:    types.AccessModeRWO,
		ReclaimPolicy: types.ReclaimPolicyDelete,
		Parameters:    map[string]string{"hostPath": hostPath},
	}
	_, err := d.Provision(context.Background(), driver.OpContext{
		Volume:     vol,
		Parameters: vol.Parameters,
	}, driver.ProvisionRequest{})
	if err == nil {
		t.Fatal("Provision: want error for reclaimPolicy=delete on local-host, got nil")
	}
	if !errors.Is(err, driver.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
}

func TestLocalHostRejectsPathOutsideAllowlist(t *testing.T) {
	allowRoot := t.TempDir()
	d := newHostDriver(t, allowRoot)
	stranger := t.TempDir() // different temp dir, not in allowlist
	vol := &types.Volume{
		Name:       "x",
		Namespace:  "default",
		AccessMode: types.AccessModeRWO,
		Parameters: map[string]string{"hostPath": stranger},
	}
	_, err := d.Provision(context.Background(), driver.OpContext{
		Volume:     vol,
		Parameters: vol.Parameters,
	}, driver.ProvisionRequest{})
	if err == nil {
		t.Fatal("Provision: want error for out-of-allowlist host path")
	}
	if !errors.Is(err, driver.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
}

func TestLocalHostRejectsMissingPathWithoutCreate(t *testing.T) {
	allowRoot := t.TempDir()
	d, err := driver.New(local.DriverNameLocalHost, map[string]any{
		"hostPathAllowlist":  []string{allowRoot},
		"allowCreateMissing": false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	missing := filepath.Join(allowRoot, "does-not-exist")
	vol := &types.Volume{
		Name:       "x",
		Namespace:  "default",
		AccessMode: types.AccessModeRWO,
		Parameters: map[string]string{"hostPath": missing},
	}
	_, perr := d.Provision(context.Background(), driver.OpContext{
		Volume:     vol,
		Parameters: vol.Parameters,
	}, driver.ProvisionRequest{})
	if !errors.Is(perr, driver.ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", perr)
	}
}

func TestLocalRejectsRWX(t *testing.T) {
	d, _ := newManagedDriver(t)
	vol := &types.Volume{Name: "v", Namespace: "default", AccessMode: types.AccessModeRWX}
	_, err := d.Provision(context.Background(), driver.OpContext{Volume: vol}, driver.ProvisionRequest{})
	if !errors.Is(err, driver.ErrAccessModeUnsupported) {
		t.Fatalf("want ErrAccessModeUnsupported, got %v", err)
	}
}

func TestLocalSnapshotRoundTrip(t *testing.T) {
	d, _ := newManagedDriver(t)
	ctx := context.Background()
	vol := &types.Volume{Name: "src", Namespace: "default", AccessMode: types.AccessModeRWO}
	srcOpCtx := driver.OpContext{Volume: vol}
	handle, err := d.Provision(ctx, srcOpCtx, driver.ProvisionRequest{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Drop a payload file directly in the volume dir.
	payload := filepath.Join(string(handle), "payload.txt")
	if err := os.WriteFile(payload, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	snap := &types.Snapshot{Name: "snap1", Namespace: "default", SourceVolume: "src"}
	snapHandle, err := d.Snapshot(ctx, srcOpCtx, driver.SnapshotRequest{Handle: handle, Snapshot: snap})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	target := &types.Volume{Name: "restored", Namespace: "default", AccessMode: types.AccessModeRWO}
	restoredOpCtx := driver.OpContext{Volume: target}
	restoredHandle, err := d.RestoreFromSnapshot(ctx, restoredOpCtx, driver.RestoreRequest{
		Source: snap, SourceHandle: snapHandle,
	})
	if err != nil {
		t.Fatalf("RestoreFromSnapshot: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(string(restoredHandle), "payload.txt"))
	if err != nil {
		t.Fatalf("read restored payload: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("payload mismatch: got %q want %q", got, "hello")
	}
}

// TestLocalHostWriteRestartRead is the RUNE-070 e2e persistence proof
// for the local-host driver: write a payload through one driver
// instance, then construct a SECOND driver against the same allowlist
// (the in-process equivalent of an agent restart) and assert the
// payload is still readable. This is the host-path twin of
// TestLocalSnapshotRoundTrip's content-survival check.
func TestLocalHostWriteRestartRead(t *testing.T) {
	allowRoot := t.TempDir()
	hostPath := filepath.Join(allowRoot, "vol-restart")
	if err := os.MkdirAll(hostPath, 0o750); err != nil {
		t.Fatalf("mkdir host path: %v", err)
	}

	mkDriver := func() driver.Driver {
		d, err := driver.New(local.DriverNameLocalHost, map[string]any{
			"hostPathAllowlist":  []string{allowRoot},
			"allowCreateMissing": false,
		})
		if err != nil {
			t.Fatalf("New(local-host): %v", err)
		}
		return d
	}

	vol := &types.Volume{
		Name:       "host-restart",
		Namespace:  "default",
		AccessMode: types.AccessModeRWO,
		Parameters: map[string]string{"hostPath": hostPath},
	}

	ctx := context.Background()

	opctx := driver.OpContext{
		Volume:     vol,
		Parameters: vol.Parameters,
	}
	d1 := mkDriver()
	handle, err := d1.Provision(ctx, opctx, driver.ProvisionRequest{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	target1, err := d1.Mount(ctx, opctx, driver.MountOpts{Handle: handle})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}

	payload := filepath.Join(string(target1), "payload.txt")
	if err := os.WriteFile(payload, []byte("survives-restart"), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := d1.Unmount(ctx, opctx, target1); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	// Fresh driver, fresh process state — same on-disk host path.
	d2 := mkDriver()
	target2, err := d2.Mount(ctx, opctx, driver.MountOpts{Handle: handle})
	if err != nil {
		t.Fatalf("Mount (post-restart): %v", err)
	}
	got, err := os.ReadFile(filepath.Join(string(target2), "payload.txt"))
	if err != nil {
		t.Fatalf("read payload after restart: %v", err)
	}
	if string(got) != "survives-restart" {
		t.Fatalf("payload mismatch after restart: got %q", got)
	}
	if err := d2.Unmount(ctx, opctx, target2); err != nil {
		t.Fatalf("Unmount (post-restart): %v", err)
	}
}

func sprintfInt(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
