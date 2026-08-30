//go:build linux

package mountsync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestUnmountFlushesWhenAnotherMountHoldsTheSuperblock is the test this
// package exists to pass.
//
// It builds the production shape on a real filesystem: an ext4 volume
// mounted where the agent mounts one, with a SECOND mount of the same
// superblock standing in for the bind a container runtime makes into its
// own mount namespace. A file is written without fsync — an ordinary
// write-back workload — and then the raw device is captured, which is
// what a detach hands back.
//
// Against a bare umount(2) this fixture loses the file — that is the
// production bug. Unmount must recover it intact.
func TestUnmountFlushesWhenAnotherMountHoldsTheSuperblock(t *testing.T) {
	requireLoopMount(t)

	for _, tc := range []struct {
		name      string
		bare      bool // tear down with a bare umount(2)
		wantBytes int
	}{
		{"bare umount(2) loses the write", true, 0},
		{"mountsync.Unmount keeps it", false, 90},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, img, target := heldMountFixture(t)

			// The workload writes and does not fsync.
			writeManifest(t, target)

			if tc.bare {
				if err := unix.Unmount(target, 0); err != nil {
					t.Fatalf("bare umount: %v", err)
				}
			} else if err := Unmount("test", target); err != nil {
				t.Fatalf("Unmount: %v", err)
			}

			if got := bytesOnDevice(t, dir, img); got != tc.wantBytes {
				t.Errorf("recovered %d bytes from the raw device, want %d", got, tc.wantBytes)
			}
		})
	}
}

// heldMountFixture builds the production shape: an ext4 volume mounted
// where the agent mounts one, with a second mount of the same superblock
// standing in for the bind a container runtime makes into its own mount
// namespace. Returns the working dir, the backing image, and the mount
// target.
func heldMountFixture(t *testing.T) (dir, img, target string) {
	t.Helper()
	dir = t.TempDir()
	img = filepath.Join(dir, "disk.img")
	target = filepath.Join(dir, "mnt")
	second := filepath.Join(dir, "held")
	for _, d := range []string{target, second} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	makeExt4(t, img)
	mustRun(t, "mount", "-o", "loop", img, target)
	t.Cleanup(func() { _ = exec.Command("umount", "-l", target).Run() })

	// Create and commit the file, so only its contents are at risk — the
	// reported signature is a correctly-named zero-length file.
	if err := os.WriteFile(filepath.Join(target, "SYSTEM"), nil, 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	mustRun(t, "sync")

	mustRun(t, "mount", "--bind", target, second)
	t.Cleanup(func() { _ = exec.Command("umount", "-l", second).Run() })
	return dir, img, target
}

// writeManifest writes the 90-byte payload without fsync — an ordinary
// write-back workload.
func writeManifest(t *testing.T, target string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(target, "SYSTEM"), []byte(strings.Repeat("0", 90)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// bytesOnDevice snapshots the backing file and reports the length of
// SYSTEM as it exists on disk — what a detached volume would come back
// with.
func bytesOnDevice(t *testing.T, dir, img string) int {
	t.Helper()
	snap := filepath.Join(dir, "snap.img")
	mustRun(t, "cp", img, snap)
	mnt := filepath.Join(dir, "snapmnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustRun(t, "mount", "-o", "loop", snap, mnt)
	defer func() { _ = exec.Command("umount", mnt).Run() }()

	fi, err := os.Stat(filepath.Join(mnt, "SYSTEM"))
	if err != nil {
		if os.IsNotExist(err) {
			return -1 // file never reached the device at all
		}
		t.Fatalf("stat: %v", err)
	}
	return int(fi.Size())
}

func makeExt4(t *testing.T, img string) {
	t.Helper()
	f, err := os.Create(img)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := f.Truncate(32 << 20); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_ = f.Close()
	mustRun(t, "mkfs.ext4", "-q", "-F", img)
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

// requireLoopMount skips unless this process can actually mount a loop
// device, which needs CAP_SYS_ADMIN, mkfs.ext4 and loop support. That is
// the production condition, not the default CI one — see the privileged
// job in the CI workflow.
func requireLoopMount(t *testing.T) {
	t.Helper()
	// The CI job that exists to run these sets this. Without it a
	// degraded runner — no CAP_SYS_ADMIN, no loop device, no mkfs —
	// would skip every one of them and the step would still go green,
	// which is the silent-skip failure this package was written to end.
	required := os.Getenv("RUNE_REQUIRE_PRIVILEGED_MOUNT") != ""
	skip := func(format string, args ...any) {
		if required {
			t.Fatalf("RUNE_REQUIRE_PRIVILEGED_MOUNT is set but "+format, args...)
		}
		t.Skipf(format, args...)
	}
	if unix.Geteuid() != 0 {
		skip("needs root to mount a loop device")
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		skip("needs mkfs.ext4")
	}
	dir := t.TempDir()
	img := filepath.Join(dir, "probe.img")
	f, err := os.Create(img)
	if err != nil {
		skip("cannot create a probe image")
	}
	_ = f.Truncate(8 << 20)
	_ = f.Close()
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", img).CombinedOutput(); err != nil {
		skip("cannot mkfs: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	mnt := filepath.Join(dir, "probe")
	_ = os.MkdirAll(mnt, 0o755)
	if out, err := exec.Command("mount", "-o", "loop", img, mnt).CombinedOutput(); err != nil {
		skip("cannot loop-mount here: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command("umount", mnt).Run()
}

// TestUnmountSymlinkedTargetStillFlushes guards the gate against the two
// syscalls that act on the path after it.
//
// syncTarget's open(2) and umount(2) both follow symlinks. A mount-point
// check that did not would answer "nothing mounted here" for a symlinked
// target, skip the flush, and then unmount successfully — losing the
// writes it was there to protect.
func TestUnmountSymlinkedTargetStillFlushes(t *testing.T) {
	requireLoopMount(t)

	dir := t.TempDir()
	img := filepath.Join(dir, "disk.img")
	real := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeExt4(t, img)
	mustRun(t, "mount", "-o", "loop", img, real)
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := os.WriteFile(filepath.Join(real, "SYSTEM"), nil, 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	mustRun(t, "sync")
	// A second reference, as a container's bind would be, so a bare
	// umount(2) cannot flush on its own.
	second := filepath.Join(dir, "held")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustRun(t, "mount", "--bind", real, second)
	t.Cleanup(func() { _ = exec.Command("umount", "-l", second).Run() })

	if err := os.WriteFile(filepath.Join(real, "SYSTEM"), []byte(strings.Repeat("0", 90)), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Tear down through the symlink.
	if err := Unmount("test", link); err != nil {
		t.Fatalf("Unmount via symlink: %v", err)
	}
	if got := bytesOnDevice(t, dir, img); got != 90 {
		t.Errorf("recovered %d bytes through a symlinked target, want 90 — the flush was skipped", got)
	}
}

// TestUnmountBusyTargetReportsFlushed covers the EBUSY branch, which had
// no test in any environment: a host-side holder keeps the mount busy, so
// umount(2) fails and the caller detaches anyway. The message is the only
// warning an operator gets, so assert its wording.
func TestUnmountBusyTargetReportsFlushed(t *testing.T) {
	requireLoopMount(t)

	dir := t.TempDir()
	img := filepath.Join(dir, "disk.img")
	target := filepath.Join(dir, "mnt")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeExt4(t, img)
	mustRun(t, "mount", "-o", "loop", img, target)
	t.Cleanup(func() { _ = exec.Command("umount", "-l", target).Run() })

	file := filepath.Join(target, "SYSTEM")
	if err := os.WriteFile(file, []byte("held"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// An open fd in this namespace is what makes umount(2) return EBUSY;
	// a bind mount does not (it is an independent reference).
	held, err := os.Open(file)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = held.Close() }()

	err = Unmount("test", target)
	if err == nil {
		t.Fatal("expected EBUSY with an open fd holding the mount")
	}
	if !strings.Contains(err.Error(), "flushed as of now") {
		t.Errorf("a busy target was flushed, and the message must say so: %v", err)
	}
	if strings.Contains(err.Error(), "nothing was flushed") {
		t.Errorf("the mount was real and was flushed; message claims otherwise: %v", err)
	}
}
