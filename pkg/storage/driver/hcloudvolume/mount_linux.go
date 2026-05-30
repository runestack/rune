//go:build linux

package hcloudvolume

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"
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

func (execMounter) Unmount(ctx context.Context, target string) error {
	if !alreadyMounted(ctx, target) {
		return nil
	}
	if err := unix.Unmount(target, 0); err != nil {
		return fmt.Errorf("hcloudvolume: umount(2) %s: %w", target, err)
	}
	return nil
}
