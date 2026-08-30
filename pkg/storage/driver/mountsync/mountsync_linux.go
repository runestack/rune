//go:build linux

package mountsync

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// syncTarget opens the mount point and calls syncfs(2) on it, flushing
// only the filesystem containing that path. sync(2) would flush every
// filesystem on the node — on a host with several volumes and a busy
// root disk that turns one volume's teardown into everyone's stall,
// inside a shutdown budget measured in seconds.
func syncTarget(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("mountsync: open %s: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Syncfs(fd); err != nil {
		return fmt.Errorf("mountsync: syncfs %s: %w", path, err)
	}
	return nil
}

func unmountTarget(driver, target string) error {
	// Flush first, and before the unmount — see the package doc.
	var (
		syncErr error
		synced  bool
	)
	if isMountPoint(target) {
		syncErr = syncTarget(target)
		synced = syncErr == nil
	}
	// No pre-probe on the unmount itself. A probe needs a subprocess
	// (findmnt), teardown runs during shutdown where a cancelled context
	// stops exec starting, and a probe that could not run reads as "not
	// mounted" — skipping the unmount entirely. umount(2) answers
	// authoritatively with no subprocess.
	err := unix.Unmount(target, 0)
	switch {
	case err == nil, notMounted(err):
		return nil
	case syncErr != nil:
		return fmt.Errorf("%s: umount(2) %s: %w (the filesystem was NOT flushed first: %v; a detach now can lose unwritten data)",
			driver, target, err, syncErr)
	case !synced:
		// Nothing was mounted here, so there was nothing to flush. Saying
		// "flushed" would assert work that never happened.
		return fmt.Errorf("%s: umount(2) %s: %w (nothing appeared to be mounted at this path, so nothing was flushed)",
			driver, target, err)
	default:
		// Not a reassurance: on this path the holder is usually a running
		// container, still writing.
		return fmt.Errorf("%s: umount(2) %s: %w (flushed as of now; writes made after this point are not on the disk)",
			driver, target, err)
	}
}

// isMountPoint reports whether target has a different filesystem from the
// directory containing it, which for these drivers means a volume is
// mounted there — they always mount a distinct block device.
//
// It exists to keep the idempotent path off the root disk: where nothing
// is mounted, syncTarget would resolve to whatever the mount root sits on
// — on a default node, the root disk — which is the whole-node stall
// syncTarget is written to avoid. Advisory only: any uncertainty answers
// "yes, sync it", because an unnecessary flush costs time and a skipped
// one costs data.
func isMountPoint(target string) bool {
	var self, parent unix.Stat_t
	// Stat, not Lstat: syncTarget's open(2) and umount(2) both follow
	// symlinks, so a gate that did not would answer "nothing mounted here"
	// for a symlinked target and skip the flush on a live volume.
	if err := unix.Stat(target, &self); err != nil {
		return true // cannot tell: flush anyway
	}
	if err := unix.Stat(target+"/..", &parent); err != nil {
		return true
	}
	return self.Dev != parent.Dev
}

// notMounted reports whether an error from umount(2) means there was
// nothing mounted at the target — the idempotent success this package
// promises.
//
// EPERM is deliberately absent. Unprivileged umount(2) fails with EPERM
// before the kernel considers whether the path is a mount point, so
// treating it as "nothing to do" would turn a missing CAP_SYS_ADMIN into
// a silent no-op — the same class of bug as the findmnt probe this
// replaced. runed holds CAP_SYS_ADMIN in production (it mounts these
// volumes), so EPERM there is a real misconfiguration and must surface.
//
// EINVAL is trusted as "not a mount point" because runed runs in the host
// mount namespace. A runed inside its own namespace would get EINVAL for a
// volume it can see but not unmount, and that would read as success here.
func notMounted(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT)
}
