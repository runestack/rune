//go:build linux

package mountsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// --- syncTarget ------------------------------------------------------

func TestSyncTargetFlushesRealDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := syncTarget(dir); err != nil {
		t.Errorf("syncTarget on a real directory should succeed, got %v", err)
	}
}

func TestSyncTargetReportsMissingPath(t *testing.T) {
	if err := syncTarget(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("syncTarget must report a path it could not open, not report success")
	}
}

// O_DIRECTORY is a sanity check that the caller passed a mount point
// rather than a file. It does not change which filesystem gets flushed —
// syncfs works through either — so this pins the guard, not a safety
// property.
func TestSyncTargetRejectsNonDirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := syncTarget(f); err == nil {
		t.Error("syncTarget on a non-directory should error rather than claim success")
	}
}

// --- isMountPoint ----------------------------------------------------

// A plain directory is not a mount point, so teardown must not flush the
// filesystem it happens to sit on — that would be the root disk, and the
// whole-node stall syncTarget exists to avoid.
func TestIsMountPointFalseForPlainDirectory(t *testing.T) {
	if isMountPoint(t.TempDir()) {
		t.Error("a plain directory must not be treated as a mount point")
	}
}

// Uncertainty resolves toward flushing: an unnecessary sync costs time, a
// skipped one costs data.
func TestIsMountPointAssumesMountedWhenItCannotTell(t *testing.T) {
	if !isMountPoint(filepath.Join(t.TempDir(), "absent")) {
		t.Error("an unstattable target must be assumed mounted, so the flush still happens")
	}
}

// --- notMounted ------------------------------------------------------

// Getting this wrong is expensive in both directions: too generous and a
// failed unmount reports success while the caller detaches a live
// filesystem; too strict and ordinary idempotent teardown looks broken.
//
// Table-driven because umount(2) needs CAP_SYS_ADMIN, which CI lacks.
func TestNotMountedClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"EINVAL — not a mount point", unix.EINVAL, true},
		{"ENOENT — target is gone", unix.ENOENT, true},
		{"EBUSY — something still holds it", unix.EBUSY, false},
		// EPERM means we could not even try. Folding it in would turn a
		// missing capability into a silent no-op.
		{"EPERM — we lack CAP_SYS_ADMIN", unix.EPERM, false},
		{"EACCES — permission on a path component", unix.EACCES, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := notMounted(c.err); got != c.want {
				t.Errorf("notMounted(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// --- Unmount ---------------------------------------------------------

// Idempotence is only observable to a privileged caller: without
// CAP_SYS_ADMIN the kernel answers EPERM before it evaluates the target.
// runed holds that capability in production; CI does not, so this skips
// rather than asserting something the environment cannot show.
func TestUnmountIsIdempotentOnNonMountpoint(t *testing.T) {
	requirePrivilegedUnmount(t)
	if err := Unmount("test", t.TempDir()); err != nil {
		t.Errorf("Unmount on a non-mountpoint must succeed (idempotent), got %v", err)
	}
}

func TestUnmountMissingTargetIsNotAnError(t *testing.T) {
	requirePrivilegedUnmount(t)
	if err := Unmount("test", filepath.Join(t.TempDir(), "never-created")); err != nil {
		t.Errorf("Unmount on a missing target should be a no-op, got %v", err)
	}
}

// The message on a failed unmount is the only warning an operator gets
// before the caller detaches anyway, so it must state what is on the disk
// rather than reassure.
func TestUnmountReportsUnflushedFilesystem(t *testing.T) {
	err := Unmount("test", filepath.Join(t.TempDir(), "absent", "deeper"))
	if err == nil {
		t.Skip("environment allowed the unmount; the both-failed branch is unreachable here")
	}
	if !strings.Contains(err.Error(), "NOT flushed") {
		t.Errorf("an unflushed failure must warn about data loss, got: %v", err)
	}
}

// requirePrivilegedUnmount skips when the process cannot call umount(2)
// at all, so a CI failure means a real regression rather than a sandbox.
func requirePrivilegedUnmount(t *testing.T) {
	t.Helper()
	if err := unix.Unmount(t.TempDir(), 0); err == unix.EPERM {
		t.Skip("umount(2) needs CAP_SYS_ADMIN; runed has it in production, this environment does not")
	}
}
