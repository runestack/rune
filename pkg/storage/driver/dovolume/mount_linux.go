//go:build linux

package dovolume

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"
)

// Mount on Linux calls mount(2) directly. /bin/mount on util-linux
// 2.39+ refuses to proceed when the calling process carries ambient
// CAP_SYS_ADMIN (its mnt_drop_permissions() heuristic flags the
// environment as "privileged but not root" and bails with -EPERM).
// The syscall has no such paranoia — with CAP_SYS_ADMIN in the
// process's effective set it just works.
func (execMounter) Mount(ctx context.Context, dev, target, fsType string, readOnly bool) error {
	if alreadyMounted(ctx, target) {
		return nil
	}
	var flags uintptr
	if readOnly {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount(dev, target, fsType, flags, ""); err != nil {
		return fmt.Errorf("dovolume: mount(2) %s -> %s (fs=%s, ro=%v): %w", dev, target, fsType, readOnly, err)
	}
	return nil
}

// Unmount on Linux calls umount2(2) directly with no flags.
func (execMounter) Unmount(ctx context.Context, target string) error {
	if !alreadyMounted(ctx, target) {
		return nil
	}
	if err := unix.Unmount(target, 0); err != nil {
		return fmt.Errorf("dovolume: umount(2) %s: %w", target, err)
	}
	return nil
}
