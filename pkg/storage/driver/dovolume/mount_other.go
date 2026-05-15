//go:build !linux

package dovolume

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Mount on non-Linux platforms shells out to /bin/mount. The driver
// isn't meaningfully usable here (DO droplets are Linux), but keeping
// the package buildable on darwin lets developers run the unit tests
// (which use fakeMounter) on a Mac.
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
