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
// Zero-length files with correct names, owners and modes is the signature
// operators see. Any workload relying on ordinary write-back is exposed;
// databases escape only because they fsync their own journals.
//
// The consequence for anyone editing this package: the sync must stay
// unconditional and must stay BEFORE the unmount. Moving it after, or
// behind an error check, reads as a cheap optimisation because umount(2)
// "already flushes" — and silently reopens the bug for every volume a
// container is holding. See issue #270.
package mountsync

// Unmount flushes the filesystem at target and then unmounts it. driver
// names the calling driver for error messages ("gcepd").
//
// When the unmount fails the error states whether the flush succeeded:
// the caller detaches either way, so that is the difference between a
// volume left attached and unwritten data discarded.
//
// Idempotent for a caller holding CAP_SYS_ADMIN — a target that is not a
// mount point, or is already gone, returns nil. Unprivileged, umount(2)
// answers EPERM before it looks at the target, and that surfaces as an
// error rather than a silent success.
func Unmount(driver, target string) error {
	return unmountTarget(driver, target)
}
