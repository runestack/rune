// Package drivertest provides a conformance test harness for storage
// Driver implementations. Built-in drivers and out-of-tree drivers alike
// run RunConformance(t, factory) to verify they implement the contract
// from pkg/storage/driver consistently.
//
// The harness exercises the full lifecycle a driver is expected to
// support — provision -> attach -> mount -> write -> unmount -> detach ->
// snapshot -> restore -> expand -> delete — gating capability-specific
// stages on the driver's advertised Capabilities.
//
// Introduced in RUNE-069. See _docs/designs/RUNE-069-Storage-Management.md.
package drivertest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/types"
)

// Config controls how the conformance suite exercises a driver.
type Config struct {
	// Driver is the instance under test. Must already be configured.
	Driver driver.Driver

	// NewVolume returns a fresh Volume + StorageClass + parameters for each
	// test case. The harness calls it multiple times. Implementations are
	// responsible for picking unique names and (for local-host style
	// drivers) writable host paths.
	NewVolume func(t *testing.T) (*types.Volume, *types.StorageClass, map[string]string)

	// NodeID is the synthetic node identifier passed to Attach/Mount.
	// Defaults to "node-test" when empty.
	NodeID driver.NodeID

	// MountTargetRoot is the directory the harness creates per-volume mount
	// targets under. Defaults to t.TempDir() when empty.
	MountTargetRoot string

	// SkipExpand skips the expand stage even if Capabilities.Expand is true
	// (useful for drivers whose backing infra can't easily resize in CI).
	SkipExpand bool

	// SkipSnapshot skips the snapshot stage even if Capabilities.Snapshots
	// is true.
	SkipSnapshot bool
}

// RunConformance runs the full conformance suite against cfg.Driver. It is
// intended to be called from a *_test.go file via t.Run("conformance", ...).
func RunConformance(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.Driver == nil {
		t.Fatal("drivertest: Config.Driver is required")
	}
	if cfg.NewVolume == nil {
		t.Fatal("drivertest: Config.NewVolume is required")
	}
	if cfg.NodeID == "" {
		cfg.NodeID = "node-test"
	}
	if cfg.MountTargetRoot == "" {
		cfg.MountTargetRoot = t.TempDir()
	}

	t.Run("Capabilities", func(t *testing.T) { testCapabilities(t, cfg) })
	t.Run("Lifecycle", func(t *testing.T) { testLifecycle(t, cfg) })
	t.Run("Idempotency", func(t *testing.T) { testIdempotency(t, cfg) })
	t.Run("UnsupportedSnapshot", func(t *testing.T) { testUnsupportedSnapshot(t, cfg) })
}

func testCapabilities(t *testing.T, cfg Config) {
	t.Helper()
	caps := cfg.Driver.Capabilities()
	if len(caps.AccessModes) == 0 {
		t.Fatal("driver advertises no access modes")
	}
	seen := make(map[types.AccessMode]struct{}, len(caps.AccessModes))
	for _, m := range caps.AccessModes {
		switch m {
		case types.AccessModeRWO, types.AccessModeROX, types.AccessModeRWX:
		default:
			t.Errorf("unknown access mode %q", m)
		}
		if _, dup := seen[m]; dup {
			t.Errorf("access mode %q listed twice", m)
		}
		seen[m] = struct{}{}
	}
	if cfg.Driver.Name() == "" {
		t.Error("driver Name() returned empty string")
	}
}

func testLifecycle(t *testing.T, cfg Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vol, class, merged := cfg.NewVolume(t)
	opctx := driver.OpContext{
		StorageClass: class,
		Volume:       vol,
		Parameters:   merged,
	}
	req := driver.ProvisionRequest{
		SizeBytes: sizeBytesOrDefault(vol.Size, 1<<20),
	}

	handle, err := cfg.Driver.Provision(ctx, opctx, req)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if handle == "" {
		t.Fatal("Provision returned empty handle")
	}
	t.Cleanup(func() {
		// Best-effort delete on cleanup — even if the test failed mid-way.
		_ = cfg.Driver.Delete(context.Background(), opctx, handle)
	})

	device, err := cfg.Driver.Attach(ctx, opctx, handle, cfg.NodeID)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	target := driver.MountTarget(filepath.Join(cfg.MountTargetRoot, vol.Name))
	mounted, err := cfg.Driver.Mount(ctx, opctx, driver.MountOpts{
		Handle: handle,
		Node:   cfg.NodeID,
		Device: device,
		Target: target,
		FsType: merged["fsType"],
	})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if mounted == "" {
		t.Fatal("Mount returned empty target")
	}

	// Write/read smoke test: drivers backed by directories should let us
	// drop a file at mounted/probe and read it back. Block-device drivers
	// can't be exercised this way without formatting, so we skip when the
	// path doesn't exist as a directory we can write to.
	if info, err := os.Stat(string(mounted)); err == nil && info.IsDir() {
		probe := filepath.Join(string(mounted), ".drivertest-probe")
		want := []byte("rune drivertest " + vol.Name)
		if err := os.WriteFile(probe, want, 0o600); err != nil {
			t.Fatalf("write probe file: %v", err)
		}
		got, err := os.ReadFile(probe)
		if err != nil {
			t.Fatalf("read probe file: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("probe file mismatch: got %q want %q", got, want)
		}
		_ = os.Remove(probe)
	}

	if err := cfg.Driver.Unmount(ctx, opctx, mounted); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if err := cfg.Driver.Detach(ctx, opctx, handle, cfg.NodeID); err != nil {
		t.Fatalf("Detach: %v", err)
	}

	caps := cfg.Driver.Capabilities()

	if caps.Snapshots && !cfg.SkipSnapshot {
		snap := &types.Snapshot{
			Name:         vol.Name + "-snap",
			Namespace:    vol.Namespace,
			SourceVolume: vol.Name,
			Driver:       cfg.Driver.Name(),
		}
		snapHandle, err := cfg.Driver.Snapshot(ctx, opctx, driver.SnapshotRequest{
			Handle: handle, Snapshot: snap,
		})
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if snapHandle == "" {
			t.Fatal("Snapshot returned empty handle")
		}
		// Restore into a fresh volume.
		restoredVol, restoredClass, restoredMerged := cfg.NewVolume(t)
		restoreOpCtx := driver.OpContext{
			StorageClass: restoredClass,
			Volume:       restoredVol,
			Parameters:   restoredMerged,
		}
		restoredHandle, err := cfg.Driver.RestoreFromSnapshot(ctx, restoreOpCtx, driver.RestoreRequest{
			Source:       snap,
			SourceHandle: snapHandle,
			SizeBytes:    sizeBytesOrDefault(restoredVol.Size, 1<<20),
		})
		if err != nil {
			t.Fatalf("RestoreFromSnapshot: %v", err)
		}
		t.Cleanup(func() { _ = cfg.Driver.Delete(context.Background(), restoreOpCtx, restoredHandle) })

		// DeleteSnapshot must be idempotent — calling twice is OK.
		if err := cfg.Driver.DeleteSnapshot(ctx, opctx, snapHandle); err != nil {
			t.Fatalf("DeleteSnapshot: %v", err)
		}
		if err := cfg.Driver.DeleteSnapshot(ctx, opctx, snapHandle); err != nil {
			t.Fatalf("DeleteSnapshot (second call must be idempotent): %v", err)
		}
	}

	if caps.Expand && !cfg.SkipExpand {
		// Expand to original size + 1MiB. Drivers that need offline expand
		// must accept this (volume is already detached).
		newSize := fmt.Sprintf("%d", req.SizeBytes+(1<<20))
		if err := cfg.Driver.Expand(ctx, opctx, handle, newSize); err != nil {
			t.Fatalf("Expand: %v", err)
		}
	}

	if err := cfg.Driver.Delete(ctx, opctx, handle); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func testIdempotency(t *testing.T, cfg Config) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	vol, class, merged := cfg.NewVolume(t)
	opctx := driver.OpContext{
		StorageClass: class,
		Volume:       vol,
		Parameters:   merged,
	}
	req := driver.ProvisionRequest{
		SizeBytes: sizeBytesOrDefault(vol.Size, 1<<20),
	}
	handle, err := cfg.Driver.Provision(ctx, opctx, req)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Provision again with the same Volume — must succeed (idempotent) and
	// return either the same handle or a logically-equivalent one.
	handle2, err := cfg.Driver.Provision(ctx, opctx, req)
	if err != nil {
		t.Fatalf("Provision (second call): %v", err)
	}
	if handle2 == "" {
		t.Fatal("second Provision returned empty handle")
	}

	// Detach when never attached — must not error.
	if err := cfg.Driver.Detach(ctx, opctx, handle, cfg.NodeID); err != nil {
		t.Fatalf("Detach (never attached): %v", err)
	}

	// Delete twice — second call must succeed.
	if err := cfg.Driver.Delete(ctx, opctx, handle); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := cfg.Driver.Delete(ctx, opctx, handle); err != nil {
		t.Fatalf("Delete (second call): %v", err)
	}
	// And clean up the duplicate handle from Provision #2 if it differs.
	if handle2 != handle {
		_ = cfg.Driver.Delete(ctx, opctx, handle2)
	}
}

func testUnsupportedSnapshot(t *testing.T, cfg Config) {
	t.Helper()
	caps := cfg.Driver.Capabilities()
	if caps.Snapshots {
		t.Skip("driver advertises snapshot support; nothing to assert here")
	}
	ctx := context.Background()
	opctx := driver.OpContext{
		Volume:     &types.Volume{Name: "doesnt-matter"},
		Parameters: map[string]string{},
	}
	_, err := cfg.Driver.Snapshot(ctx, opctx, driver.SnapshotRequest{
		Snapshot: &types.Snapshot{Name: "doesnt-matter"},
	})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("Snapshot on snapshotless driver: want ErrUnsupported, got %v", err)
	}
}

// sizeBytesOrDefault parses the human-readable size string very loosely;
// real callers go through the controller's parser. The harness only needs
// a reasonable byte count for ProvisionRequest.SizeBytes.
func sizeBytesOrDefault(_ string, fallback int64) int64 {
	// Conformance tests pass through whatever the test harness wants; we
	// don't bother parsing here. Real size parsing lives in the controller.
	return fallback
}
