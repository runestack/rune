// Package mountsync owns the teardown half of a block-volume mount:
// flushing the filesystem and unmounting it.
//
// It exists because detaching a cloud disk discards whatever is still in
// the page cache. A successful umount(2) flushes on its own, so the sync
// is redundant on the happy path — it matters on the paths where the
// unmount does not happen:
//
//   - The unmount fails. A container still holding the bind returns
//     EBUSY, and the agent detaches anyway rather than strand the volume
//     on a node that is going away.
//   - The disk is detached by something outside this process.
//
// In those cases an unsynced ext4 comes back with its metadata journaled
// and its file contents gone: files with the right name, owner and mode,
// and a length of zero. That was issue #270.
//
// The four cloud drivers (do-volume, gce-pd, aws-ebs, hcloud-volume) had
// byte-identical copies of this logic. They share this one now, because
// a subtle difference between copies of a data-loss-critical routine is
// not something a reviewer would reliably catch.
package mountsync

// Target flushes every dirty page of the filesystem containing path.
//
// Best-effort by design: on a teardown path the useful response to
// "could not sync" is to carry on and report it, not to abandon the
// unmount. A nil return means the filesystem was flushed.
func Target(path string) error {
	return syncTarget(path)
}

// Unmount flushes the filesystem at target and then unmounts it.
//
// driver names the calling driver for error messages ("gcepd"). The
// returned error says explicitly whether the flush succeeded, because
// the caller detaches the disk either way: that is the difference
// between a volume left attached somewhere and unwritten data discarded.
//
// Idempotent — unmounting a path that is not a mount point, or that no
// longer exists, is a nil return.
func Unmount(driver, target string) error {
	return unmountTarget(driver, target)
}
