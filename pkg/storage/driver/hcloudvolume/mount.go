package hcloudvolume

import (
	"context"
	"os"
	"path/filepath"

	"github.com/runestack/rune/pkg/storage/driver/blockfs"
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

// EnsureFormatted formats dev only if blkid shows it carries nothing;
// see pkg/storage/driver/blockfs.
func (execMounter) EnsureFormatted(ctx context.Context, dev, fsType string) error {
	return blockfs.EnsureFormatted(ctx, "hcloudvolume", dev, fsType)
}
