package hcloudvolume

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
	EnsureFormatted(ctx context.Context, dev, fsType string) error
	Mount(ctx context.Context, dev, target, fsType string, readOnly bool) error
	Unmount(ctx context.Context, target string) error
	MkdirAll(target string, mode os.FileMode) error
}

// execMounter is the production mountExec. See dovolume/mount.go for
// the rationale behind calling mount(2) directly on Linux (the
// util-linux 2.39+ "ambient CAP_SYS_ADMIN" footgun applies equally
// here).
type execMounter struct{}

func (execMounter) MkdirAll(target string, mode os.FileMode) error {
	return os.MkdirAll(filepath.Clean(target), mode)
}

func (execMounter) EnsureFormatted(ctx context.Context, dev, fsType string) error {
	if fsType == "" {
		return errors.New("hcloudvolume: EnsureFormatted called with empty fsType")
	}
	out, err := exec.CommandContext(ctx, "lsblk", "-no", "FSTYPE", dev).Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return nil
	}
	mkfs := "mkfs." + fsType
	if _, err := exec.LookPath(mkfs); err != nil {
		return fmt.Errorf("hcloudvolume: %s not in PATH: %w", mkfs, err)
	}
	cmd := exec.CommandContext(ctx, mkfs, dev)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("hcloudvolume: %s %s: %w (%s)", mkfs, dev, err, strings.TrimSpace(string(combined)))
	}
	return nil
}

func alreadyMounted(ctx context.Context, target string) bool {
	cmd := exec.CommandContext(ctx, "findmnt", "-n", target)
	return cmd.Run() == nil
}
