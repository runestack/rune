//go:build linux

package mountsync

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// syncTarget opens the mount point and calls syncfs(2) on it, which
// flushes only the filesystem containing that path. sync(2) would flush
// every filesystem on the node — on a host with several volumes and a
// busy root disk that turns one volume's teardown into everyone's stall,
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
	// Flush first. See the package doc: the caller detaches even when the
	// unmount below fails, and a detach drops whatever is still cached.
	syncErr := syncTarget(target)

	// Attempt the unmount unconditionally rather than probing first. The
	// drivers used to shell out to `findmnt` to decide whether to bother —
	// but exec cannot start on an expired or cancelled context, and a
	// probe that failed to run was read as "not mounted", silently
	// skipping the unmount entirely. Teardown runs during shutdown, which
	// is exactly where contexts expire. umount(2) itself is the
	// authoritative answer and needs no subprocess.
	err := unix.Unmount(target, 0)
	switch {
	case err == nil:
		return nil
	case notMounted(err):
		return nil
	case syncErr != nil:
		return fmt.Errorf("%s: umount(2) %s: %w (filesystem was NOT flushed first: %v; detaching now can lose unwritten data)",
			driver, target, err, syncErr)
	default:
		return fmt.Errorf("%s: umount(2) %s: %w (filesystem was flushed, so a detach will not lose data)",
			driver, target, err)
	}
}

// notMounted reports whether an error from umount(2) means there was
// nothing mounted at the target — the idempotent success this package
// promises.
//
// EPERM is deliberately NOT in this set. Unprivileged umount(2) fails
// with EPERM before the kernel ever considers whether the path is a
// mount point, so treating it as "nothing to do" would turn a missing
// CAP_SYS_ADMIN into a silent no-op — the same class of bug as the
// findmnt probe this replaced. runed holds CAP_SYS_ADMIN in production
// (it mounts these volumes in the first place), so EPERM there is a real
// misconfiguration and must surface.
func notMounted(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT)
}
