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
}

// execMounter is the production mountExec. It shells out to the
// standard Linux storage utilities. Each method tolerates "already
// done" outcomes so retries are safe.
type execMounter struct{}

func (execMounter) MkdirAll(target string, mode os.FileMode) error {
	return os.MkdirAll(filepath.Clean(target), mode)
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

// Mount runs `mount -t <fsType> [-o ro] <dev> <target>`. If target is
// already a mountpoint (findmnt succeeds) we treat it as success.
func (execMounter) Mount(ctx context.Context, dev, target, fsType string, readOnly bool) error {
	if alreadyMounted(ctx, target) {
		return nil
	}
	args := []string{"-t", fsType}
	if readOnly {
		args = append(args, "-o", "ro")
	}
	args = append(args, dev, target)
	cmd := exec.CommandContext(ctx, "mount", args...)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("dovolume: mount %s -> %s: %w (%s)", dev, target, err, strings.TrimSpace(string(combined)))
	}
	return nil
}

func (execMounter) Unmount(ctx context.Context, target string) error {
	if !alreadyMounted(ctx, target) {
		return nil
	}
	cmd := exec.CommandContext(ctx, "umount", target)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("dovolume: umount %s: %w (%s)", target, err, strings.TrimSpace(string(combined)))
	}
	return nil
}

func alreadyMounted(ctx context.Context, target string) bool {
	cmd := exec.CommandContext(ctx, "findmnt", "-n", target)
	return cmd.Run() == nil
}
