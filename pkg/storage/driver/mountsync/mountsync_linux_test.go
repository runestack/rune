//go:build linux

package mountsync

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// --- Target (syncfs) -------------------------------------------------

// Target must succeed on the ordinary path. If it errored routinely,
// every teardown would report "filesystem was NOT flushed" and the
// warning would stop meaning anything.
func TestTargetFlushesRealDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("data"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Target(dir); err != nil {
		t.Errorf("Target on a real directory should succeed, got %v", err)
	}
}

// The caller uses this error to decide whether a detach is safe, so a
// path it could not open must never look like a successful flush.
func TestTargetReportsMissingPath(t *testing.T) {
	if err := Target(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("Target must report a path it could not open, not report success")
	}
}

// O_DIRECTORY guards against being handed a file path by mistake, where
// syncing would flush the wrong thing and claim success.
func TestTargetRejectsNonDirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Target(f); err == nil {
		t.Error("Target on a non-directory should error rather than claim success")
	}
}

// --- notMounted classification ---------------------------------------

// The classifier decides whether a failed umount(2) means "nothing was
// mounted" (success) or a real failure. Getting it wrong in either
// direction is expensive: too generous and a failed unmount is reported
// as done while the caller detaches a live filesystem; too strict and
// ordinary idempotent teardown looks broken.
//
// This is table-driven rather than syscall-driven because umount(2)
// requires CAP_SYS_ADMIN — see TestUnmountRequiresPrivilegeToBeMeaningful.
func TestNotMountedClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"EINVAL — not a mount point", unix.EINVAL, true},
		{"ENOENT — target is gone", unix.ENOENT, true},
		{"EBUSY — something still holds it", unix.EBUSY, false},
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

// TestNotMountedRejectsEPERM pins the subtlest case with its reason.
//
// Unprivileged umount(2) returns EPERM *before* the kernel considers
// whether the path is a mount point. Folding EPERM into "nothing was
// mounted" would turn a missing capability into a silent no-op: unmount
// reports success, the caller detaches, and the page cache goes with it.
// That is precisely the shape of the findmnt probe this package replaced
// (issue #191), so it is worth its own test rather than one row.
func TestNotMountedRejectsEPERM(t *testing.T) {
	if notMounted(unix.EPERM) {
		t.Error("EPERM must not be read as 'nothing was mounted' — it means we could not even try")
	}
}

// --- Unmount ---------------------------------------------------------

// Unmount promises idempotence, but only a privileged caller can observe
// it: without CAP_SYS_ADMIN the kernel answers EPERM before evaluating
// the target. runed holds that capability in production. CI does not, so
// this skips rather than asserting something the environment cannot show.
func TestUnmountIsIdempotentOnNonMountpoint(t *testing.T) {
	requirePrivilegedUnmount(t)
	if err := Unmount("test", t.TempDir()); err != nil {
		t.Errorf("Unmount on a non-mountpoint must succeed (idempotent), got %v", err)
	}
}

// The regression for the silent no-op (issue #191, and a contributor to
// the data loss in #270): Unmount used to probe with findmnt via
// exec.CommandContext. On a cancelled context that process cannot start,
// the failure was read as "not mounted", and Unmount returned nil having
// done nothing — after which the caller detached the disk. Teardown runs
// during shutdown, which is exactly where contexts die.
//
// Unmount no longer takes a context or shells out, so there is nothing
// left to fail this way. The test that would once have caught the bug is
// now a compile-time property; what remains observable is that a dead
// context is simply not part of the signature.
func TestUnmountDoesNotDependOnAContext(t *testing.T) {
	requirePrivilegedUnmount(t)
	// No ctx argument exists to cancel. Calling it during "shutdown"
	// behaves the same as calling it at any other time.
	if err := Unmount("test", t.TempDir()); err != nil {
		t.Errorf("Unmount must not depend on a live context, got %v", err)
	}
}

// A target that no longer exists is already-gone, not an error.
func TestUnmountMissingTargetIsNotAnError(t *testing.T) {
	requirePrivilegedUnmount(t)
	gone := filepath.Join(t.TempDir(), "never-created")
	if err := Unmount("test", gone); err != nil {
		t.Errorf("Unmount on a missing target should be a no-op, got %v", err)
	}
}

// TestUnmountReportsUnflushedFilesystem: when the flush fails and the
// unmount fails, the error must say the data is at risk. The caller
// detaches regardless, so this sentence is the only warning an operator
// gets that a detach is about to discard writes.
func TestUnmountReportsUnflushedFilesystem(t *testing.T) {
	// A path that cannot be opened fails the sync, and (unprivileged)
	// fails the unmount too — the both-failed branch.
	err := Unmount("test", filepath.Join(t.TempDir(), "absent", "deeper"))
	if err == nil {
		t.Skip("environment allowed the unmount; the both-failed branch is unreachable here")
	}
	if !contains(err.Error(), "NOT flushed") {
		t.Errorf("an unflushed failure must warn about data loss, got: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// requirePrivilegedUnmount skips when the process cannot call umount(2)
// at all, so a CI failure means a real regression rather than a sandbox.
func requirePrivilegedUnmount(t *testing.T) {
	t.Helper()
	err := unix.Unmount(t.TempDir(), 0)
	if err == unix.EPERM {
		t.Skip("umount(2) needs CAP_SYS_ADMIN; runed has it in production, this environment does not")
	}
}
