//go:build linux

package hcloudvolume

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/runestack/rune/pkg/storage/driver/mountsync"
)

func (execMounter) Mount(ctx context.Context, dev, target, fsType string, readOnly bool) error {
	if alreadyMounted(ctx, target) {
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

func (execMounter) Unmount(_ context.Context, target string) error {
	// Flush-then-unmount lives in mountsync, shared by every cloud driver:
	// a detach discards unflushed pages, and four private copies of that
	// logic is four chances for one to drift. See issue #270.
	return mountsync.Unmount("hcloudvolume", target)
}
