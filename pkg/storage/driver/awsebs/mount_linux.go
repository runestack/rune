//go:build linux

package awsebs

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/runestack/rune/pkg/storage/driver/mountsync"
)

// Mount on Linux calls mount(2) directly. /bin/mount on util-linux 2.39+
// refuses to proceed when the calling process carries ambient
// CAP_SYS_ADMIN; the syscall has no such paranoia.
func (execMounter) Mount(ctx context.Context, dev, target, fsType string, readOnly bool) error {
	if alreadyMounted(ctx, target) {
		return nil
	}
	var flags uintptr
	if readOnly {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount(dev, target, fsType, flags, ""); err != nil {
		return fmt.Errorf("awsebs: mount(2) %s -> %s (fs=%s, ro=%v): %w", dev, target, fsType, readOnly, err)
	}
	return nil
}

// Unmount on Linux flushes the filesystem, then calls umount2(2).
func (execMounter) Unmount(_ context.Context, target string) error {
	// Flush-then-unmount lives in mountsync, shared by every cloud driver:
	// a detach discards unflushed pages, and four private copies of that
	// logic is four chances for one to drift. See issue #270.
	return mountsync.Unmount("awsebs", target)
}
