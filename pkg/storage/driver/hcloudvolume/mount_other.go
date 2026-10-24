//go:build !linux

package hcloudvolume

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

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
		return fmt.Errorf("hcloudvolume: mount %s -> %s: %w (%s)", dev, target, err, strings.TrimSpace(string(combined)))
	}
	return nil
}

func (execMounter) Unmount(ctx context.Context, target string) error {
	if !alreadyMounted(ctx, target) {
		return nil
	}
	cmd := exec.CommandContext(ctx, "umount", target)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("hcloudvolume: umount %s: %w (%s)", target, err, strings.TrimSpace(string(combined)))
	}
	return nil
}

// alreadyMounted is the non-Linux probe, for local development only: a
// findmnt that cannot run reads as "not mounted", so Unmount here can
// report success without unmounting. Linux decides by device through
// mountinfo instead.
func alreadyMounted(ctx context.Context, target string) bool {
	cmd := exec.CommandContext(ctx, "findmnt", "-n", target)
	return cmd.Run() == nil
}
