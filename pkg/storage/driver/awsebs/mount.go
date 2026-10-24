package awsebs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/runestack/rune/pkg/storage/driver/blockfs"
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

// EnsureFormatted formats dev only if blkid shows it carries nothing;
// see pkg/storage/driver/blockfs.
func (execMounter) EnsureFormatted(ctx context.Context, dev, fsType string) error {
	return blockfs.EnsureFormatted(ctx, "awsebs", dev, fsType)
}
