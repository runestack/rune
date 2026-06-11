package local

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/runestack/rune/pkg/storage/driver"
)

// Usage implements driver.UsageReporter for the managed "local" driver:
// a bounded recursive walk of the volume's backing directory. Capacity is
// reported as 0 (unknown) — directory-backed volumes have no device
// boundary, so callers fall back to the volume's declared spec size.
func (d *managedDriver) Usage(ctx context.Context, _ driver.OpContext, handle driver.VolumeHandle) (uint64, uint64, error) {
	used, err := duDir(ctx, string(handle))
	return used, 0, err
}

// Usage implements driver.UsageReporter for the "local-host" driver. Same
// directory-walk semantics as the managed driver — the handle is the
// operator-owned host path.
func (d *hostDriver) Usage(ctx context.Context, _ driver.OpContext, handle driver.VolumeHandle) (uint64, uint64, error) {
	used, err := duDir(ctx, string(handle))
	return used, 0, err
}

// Compile-time capability checks.
var (
	_ driver.UsageReporter = (*managedDriver)(nil)
	_ driver.UsageReporter = (*hostDriver)(nil)
)

// duDir sums regular-file sizes under root, checking ctx cancellation
// every few hundred entries so a huge tree can't stall an API request —
// on cancellation it returns what it has counted so far (an undercount,
// which the caller treats as best-effort).
func duDir(ctx context.Context, root string) (uint64, error) {
	if root == "" {
		return 0, os.ErrNotExist
	}
	// Stat up front: WalkDir reports a missing root through the callback,
	// where the skip-unreadable-entries policy below would swallow it.
	if _, err := os.Stat(root); err != nil {
		return 0, err
	}
	var total uint64
	var n int
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable entries rather than failing the whole walk.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil //nolint:nilerr // best-effort sum
		}
		if n++; n%256 == 0 {
			select {
			case <-ctx.Done():
				return filepath.SkipAll
			default:
			}
		}
		if d.Type().IsRegular() {
			if info, ierr := d.Info(); ierr == nil {
				// Size() is int64 and never negative for a regular file,
				// but guard the conversion anyway (gosec G115).
				if sz := info.Size(); sz > 0 {
					total += uint64(sz)
				}
			}
		}
		return nil
	})
	if err != nil {
		return total, err
	}
	return total, nil
}
