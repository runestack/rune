//go:build linux

package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckFloor(t *testing.T) {
	dir := t.TempDir()
	floorPath := filepath.Join(dir, "version-floor")
	a := &Applier{Runtime: ApplierRuntime{CurrentVersion: "v0.0.1-dev.150", FloorPath: floorPath}}

	// Downgrade without the opt-in is refused even with no floor.
	err := a.checkFloor(&Manifest{Version: "v0.0.1-dev.140"})
	if err == nil || !strings.Contains(err.Error(), "did not opt into a downgrade") {
		t.Fatalf("downgrade without opt-in: %v", err)
	}

	// Absent floor + upgrade: allowed.
	if err := a.checkFloor(&Manifest{Version: "v0.0.1-dev.151"}); err != nil {
		t.Fatalf("absent floor upgrade: %v", err)
	}

	// Below-floor is refused even with the opt-in; the error carries the
	// exact root remedy.
	if err := WriteFloor(floorPath, "v0.0.1-dev.145"); err != nil {
		t.Fatal(err)
	}
	err = a.checkFloor(&Manifest{Version: "v0.0.1-dev.140", AllowDowngrade: true})
	if err == nil || !strings.Contains(err.Error(), floorPath) {
		t.Fatalf("below floor: %v", err)
	}

	// At-or-above floor downgrade with opt-in: allowed.
	if err := a.checkFloor(&Manifest{Version: "v0.0.1-dev.146", AllowDowngrade: true}); err != nil {
		t.Fatalf("above-floor downgrade with opt-in: %v", err)
	}

	// Corrupt floor fails closed.
	if err := os.WriteFile(floorPath, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.checkFloor(&Manifest{Version: "v0.0.1-dev.151"}); err == nil {
		t.Fatal("unparseable floor must refuse")
	}
}

func TestTargetRequiresReady(t *testing.T) {
	// Upgrades demand the ready flag; downgrades may target pre-ready
	// builds and must not (they would always roll back).
	if !targetRequiresReady("v0.0.1-dev.151", "v0.0.1-dev.150") {
		t.Fatal("upgrade must require ready")
	}
	if targetRequiresReady("v0.0.1-dev.140", "v0.0.1-dev.150") {
		t.Fatal("downgrade must not require ready")
	}
	if !targetRequiresReady("v0.0.1-dev.151", "dev") {
		t.Fatal("unparseable current must default to requiring ready")
	}
}

// TestConsume_ErrorStillConsumesReady pins the no-refire contract: a
// consume that fails (here: ready exists but the manifest is missing)
// must still remove `ready`, or systemd refires the oneshot in a loop
// until the path unit trips into failed state and the feature is wedged.
func TestConsume_ErrorStillConsumesReady(t *testing.T) {
	staging := t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, readyName), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Applier{StagingDir: staging, Runtime: ApplierRuntime{CurrentVersion: "v0.0.1-dev.150", Now: time.Now}}
	if _, _, err := a.consume(); err == nil {
		t.Fatal("consume with no manifest must error")
	}
	if _, err := os.Stat(filepath.Join(staging, readyName)); !os.IsNotExist(err) {
		t.Fatal("a failed consume must still remove the ready trigger")
	}
}
