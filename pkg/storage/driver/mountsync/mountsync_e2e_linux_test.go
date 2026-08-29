//go:build linux

package mountsync

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestUnmountFlushesWhenAnotherMountHoldsTheSuperblock is the test this
// package exists to pass, and the only one that fails against the code
// this replaced.
//
// It builds the production shape on a real filesystem: an ext4 volume
// mounted where the agent mounts one, with a SECOND mount of the same
// superblock standing in for the bind a container runtime makes into its
// own mount namespace. A file is written without fsync — an ordinary
// write-back workload — and then the raw device is captured, which is
// what a detach hands back.
//
// A bare umount(2) returns success here and flushes nothing, because it
// releases only one of two references to the superblock. That is the
// production bug: the agent logged "Volume unmounted" and detached a
// filesystem whose pages were still dirty. Unmount must recover the file
// intact where a bare umount(2) loses it.
func TestUnmountFlushesWhenAnotherMountHoldsTheSuperblock(t *testing.T) {
	requireLoopMount(t)

	for _, tc := range []struct {
		name      string
		bare      bool // tear down with a bare umount(2), as the old code did
		wantBytes int
	}{
		{"bare umount(2) loses the write", true, 0},
		{"mountsync.Unmount keeps it", false, 90},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			img := filepath.Join(dir, "disk.img")
			target := filepath.Join(dir, "mnt")
			second := filepath.Join(dir, "held")
			for _, d := range []string{target, second} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			}
			makeExt4(t, img)
			mustRun(t, "mount", "-o", "loop", img, target)

			// Create and commit the file, so only its contents are at risk —
			// the reported signature is a correctly-named zero-length file.
			if err := os.WriteFile(filepath.Join(target, "SYSTEM"), nil, 0o644); err != nil {
				t.Fatalf("create: %v", err)
			}
			mustRun(t, "sync")

			// The container's bind: a second reference to the superblock.
			mustRun(t, "mount", "--bind", target, second)
			t.Cleanup(func() { _ = exec.Command("umount", "-l", second).Run() })

			// The workload writes and does not fsync.
			if err := os.WriteFile(filepath.Join(target, "SYSTEM"), []byte(strings.Repeat("0", 90)), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			if tc.bare {
				if err := unix.Unmount(target, 0); err != nil {
					t.Fatalf("bare umount: %v", err)
				}
			} else if err := Unmount("test", target); err != nil {
				t.Fatalf("Unmount: %v", err)
			}

			got := bytesOnDevice(t, dir, img)
			if got != tc.wantBytes {
				t.Errorf("recovered %d bytes from the raw device, want %d", got, tc.wantBytes)
			}
		})
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
	if unix.Geteuid() != 0 {
		t.Skip("needs root to mount a loop device")
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("needs mkfs.ext4")
	}
	dir := t.TempDir()
	img := filepath.Join(dir, "probe.img")
	f, err := os.Create(img)
	if err != nil {
		t.Skip("cannot create a probe image")
	}
	_ = f.Truncate(8 << 20)
	_ = f.Close()
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", img).CombinedOutput(); err != nil {
		t.Skipf("cannot mkfs: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	mnt := filepath.Join(dir, "probe")
	_ = os.MkdirAll(mnt, 0o755)
	if out, err := exec.Command("mount", "-o", "loop", img, mnt).CombinedOutput(); err != nil {
		t.Skipf("cannot loop-mount here: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command("umount", mnt).Run()
	_ = fmt.Sprint()
}
