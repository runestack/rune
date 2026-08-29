// Package mountsync flushes a mounted filesystem and unmounts it, for the
// cloud volume drivers that then detach the underlying disk.
//
// The flush is not belt-and-braces. umount(2) writes the filesystem out
// only when it releases the LAST reference to the superblock, and a
// container started with this volume holds a second one: the runtime
// binds the mount into the container's own mount namespace. So the
// agent's umount(2) returns success, flushes nothing, and the detach
// that follows discards every page still dirty.
//
// Measured on loop-backed ext4, writing a 90-byte file without fsync and
// then reading back the raw device (what a detach hands you):
//
//	single mount, bare umount(2)         rc=0   90 bytes
//	second mount held, bare umount(2)    rc=0    0 bytes
//
// Zero-length files with correct names, owners and modes is the
// signature operators see.
//
// The consequence for anyone editing this package: the flush must stay
// BEFORE the unmount and must never become conditional on the unmount —
// moving it after, or behind an error check, reads as a cheap
// optimisation because umount(2) "already flushes", and silently
// reopens the bug for every volume a container is holding. The one
// guard that is safe is isMountPoint, which decides only whether there
// is a volume filesystem here to flush at all. See issue #270.
package mountsync

// Unmount flushes the filesystem at target and then unmounts it. driver
// names the calling driver for error messages ("gcepd").
//
// When the unmount fails the error states whether the flush succeeded:
// the caller detaches either way, so that is the difference between a
// volume left attached and unwritten data discarded.
//
// The flush is deliberately unbounded: there is no context parameter and
// no timeout. Cutting it short means detaching on a half-written
// filesystem, which is the failure this package exists to prevent, so a
// slow flush is the correct behaviour and not something to "fix" with a
// deadline. Note the ceiling is set elsewhere regardless — systemd's
// TimeoutStopSec then SIGKILL, and SIGKILL does not interrupt an
// in-flight syncfs.
//
// Idempotent for a caller holding CAP_SYS_ADMIN — a target that is not a
// mount point, or is already gone, returns nil. Unprivileged, umount(2)
// answers EPERM before it looks at the target, and that surfaces as an
// error rather than a silent success.
func Unmount(driver, target string) error {
	return unmountTarget(driver, target)
}
