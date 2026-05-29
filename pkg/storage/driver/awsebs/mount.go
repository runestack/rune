package awsebs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// mountExec is the small abstraction over the bits of the OS the driver
// needs at Mount/Unmount time. Production uses execMounter; tests inject
// a fake to avoid running real mkfs/mount/umount. Mirrors the do-volume
// driver's mountExec.
type mountExec interface {
	// EnsureFormatted formats `dev` with `fsType` if it does not already
	// carry a recognised filesystem. Idempotent.
	EnsureFormatted(ctx context.Context, dev, fsType string) error

	// Mount block-mounts dev at target with the given fsType. readOnly
	// turns the mount read-only. Idempotent on already-mounted targets.
	Mount(ctx context.Context, dev, target, fsType string, readOnly bool) error

	// Unmount unmounts target. Idempotent.
	Unmount(ctx context.Context, target string) error

	// MkdirAll creates the target directory (and parents) with mode.
	MkdirAll(target string, mode os.FileMode) error
}

// execMounter is the production mountExec. Mount/Unmount use the
// mount(2) / umount2(2) syscalls directly on Linux (see mount_linux.go) —
// shelling out to /bin/mount via util-linux 2.39+ fails with "drop
// permissions failed" when the calling process carries ambient
// CAP_SYS_ADMIN. Formatting (mkfs.<fs>) stays as shell-out.
type execMounter struct{}

func (execMounter) MkdirAll(target string, mode os.FileMode) error {
	return os.MkdirAll(filepath.Clean(target), mode)
}

// EnsureFormatted: lsblk reports the current FSTYPE; if non-empty, leave
// it alone. Otherwise run `mkfs.<fsType> <dev>`.
func (execMounter) EnsureFormatted(ctx context.Context, dev, fsType string) error {
	if fsType == "" {
		return errors.New("awsebs: EnsureFormatted called with empty fsType")
	}
	out, err := exec.CommandContext(ctx, "lsblk", "-no", "FSTYPE", dev).Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return nil // already formatted (any fs)
	}
	mkfs := "mkfs." + fsType
	if _, err := exec.LookPath(mkfs); err != nil {
		return fmt.Errorf("awsebs: %s not in PATH: %w", mkfs, err)
	}
	cmd := exec.CommandContext(ctx, mkfs, dev)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("awsebs: %s %s: %w (%s)", mkfs, dev, err, strings.TrimSpace(string(combined)))
	}
	return nil
}

func alreadyMounted(ctx context.Context, target string) bool {
	cmd := exec.CommandContext(ctx, "findmnt", "-n", target)
	return cmd.Run() == nil
}
