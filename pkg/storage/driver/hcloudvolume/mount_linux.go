//go:build linux

package hcloudvolume

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/runestack/rune/pkg/storage/driver/mountsync"
	"github.com/runestack/rune/pkg/storage/mountinfo"
)

func (execMounter) Mount(_ context.Context, dev, target, fsType string, readOnly bool) error {
	// By device, not by "anything is mounted there": another disk at the
	// target is an error, and so is a mount table that cannot be read.
	mounted, err := mountinfo.MountedAt(target, dev)
	if err != nil {
		return fmt.Errorf("hcloudvolume: check %s before mounting %s: %w", target, dev, err)
	}
	if mounted {
		return nil
	}
	var flags uintptr
	if readOnly {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount(dev, target, fsType, flags, ""); err != nil {
		return fmt.Errorf("hcloudvolume: mount(2) %s -> %s (fs=%s, ro=%v): %w", dev, target, fsType, readOnly, err)
	}
	return nil
}

// Unmount on Linux flushes the filesystem, then calls umount2(2).
func (execMounter) Unmount(_ context.Context, target string) error {
	return mountsync.Unmount("hcloudvolume", target)
}
