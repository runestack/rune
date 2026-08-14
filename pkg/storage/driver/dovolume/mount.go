package dovolume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// mountExec is the small abstraction over the bits of the OS the
// driver needs at Mount/Unmount time. Production uses execMounter;
// tests inject a fake to avoid running real mkfs/mount/umount.
type mountExec interface {
	// EnsureFormatted formats `dev` with `fsType` if it does not already
	// carry a recognised filesystem. Idempotent: a noop on a device
	// that already has a filesystem of any type.
	EnsureFormatted(ctx context.Context, dev, fsType string) error

	// Mount bind-/block-mounts dev at target with the given fsType.
	// readOnly turns the mount read-only. Idempotent on already-mounted
	// targets.
	Mount(ctx context.Context, dev, target, fsType string, readOnly bool) error

	// Unmount unmounts target. Idempotent: returns nil for paths that
	// are not mounted.
	Unmount(ctx context.Context, target string) error

	// MkdirAll creates the target directory (and parents) with mode.
	MkdirAll(target string, mode os.FileMode) error

	// DeviceExists reports whether dev resolves to a device node on this
	// host. DigitalOcean only creates the /dev/disk/by-id/scsi-0DO_Volume_*
	// link for volumes attached to THIS droplet, so its presence is a
	// local, API-free proof of attachment — which is what lets Attach skip
	// the provider round-trip after a reboot.
	DeviceExists(dev string) bool
}

// execMounter is the production mountExec. Mount/Unmount use the
// mount(2) / umount(2) syscalls directly on Linux (see mount_linux.go) —
// shelling out to /bin/mount via util-linux 2.39+ fails with
// "drop permissions failed" when the calling process carries ambient
// CAP_SYS_ADMIN, even though the cap is sufficient to perform the
// mount. Formatting (mkfs.<fs>) stays as shell-out: one-shot per
// volume and not subject to the same paranoia.
type execMounter struct{}

func (execMounter) MkdirAll(target string, mode os.FileMode) error {
	return os.MkdirAll(filepath.Clean(target), mode)
}

// DeviceExists stats the path, following the by-id symlink to the real
// device node. A dangling link (volume detached while the link lingers)
// therefore reports false, which is the conservative answer: we fall back
// to asking the provider.
func (execMounter) DeviceExists(dev string) bool {
	if dev == "" {
		return false
	}
	_, err := os.Stat(filepath.Clean(dev))
	return err == nil
}

// EnsureFormatted: lsblk reports the current FSTYPE; if non-empty,
// we leave it alone. Otherwise we run `mkfs.<fsType> <dev>`.
func (execMounter) EnsureFormatted(ctx context.Context, dev, fsType string) error {
	if fsType == "" {
		return errors.New("dovolume: EnsureFormatted called with empty fsType")
	}
	out, err := exec.CommandContext(ctx, "lsblk", "-no", "FSTYPE", dev).Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		// Already formatted (any fs).
		return nil
	}
	mkfs := "mkfs." + fsType
	if _, err := exec.LookPath(mkfs); err != nil {
		return fmt.Errorf("dovolume: %s not in PATH: %w", mkfs, err)
	}
	cmd := exec.CommandContext(ctx, mkfs, dev)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("dovolume: %s %s: %w (%s)", mkfs, dev, err, strings.TrimSpace(string(combined)))
	}
	return nil
}

func alreadyMounted(ctx context.Context, target string) bool {
	cmd := exec.CommandContext(ctx, "findmnt", "-n", target)
	return cmd.Run() == nil
}
