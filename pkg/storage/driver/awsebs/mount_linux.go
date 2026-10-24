//go:build linux

package awsebs

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/runestack/rune/pkg/storage/driver/mountsync"
	"github.com/runestack/rune/pkg/storage/mountinfo"
)

// Mount on Linux calls mount(2) directly. /bin/mount on util-linux 2.39+
// refuses to proceed when the calling process carries ambient
// CAP_SYS_ADMIN; the syscall has no such paranoia.
func (execMounter) Mount(_ context.Context, dev, target, fsType string, readOnly bool) error {
	// By device, not by "anything is mounted there": another disk at the
	// target is an error, and so is a mount table that cannot be read.
	mounted, err := mountinfo.MountedAt(target, dev)
	if err != nil {
		return fmt.Errorf("awsebs: check %s before mounting %s: %w", target, dev, err)
	}
	if mounted {
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
	return mountsync.Unmount("awsebs", target)
}
