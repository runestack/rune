//go:build linux

package awsebs

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"
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

// Unmount on Linux calls umount2(2) directly with no flags.
func (execMounter) Unmount(ctx context.Context, target string) error {
	if !alreadyMounted(ctx, target) {
		return nil
	}
	if err := unix.Unmount(target, 0); err != nil {
		return fmt.Errorf("awsebs: umount(2) %s: %w", target, err)
	}
	return nil
}
