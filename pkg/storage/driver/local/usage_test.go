package local

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/runestack/rune/pkg/storage/driver"
)

// TestDuDir sums regular files recursively and ignores directories.
func TestDuDir(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.bin"), 1000)
	mustWrite(t, filepath.Join(root, "sub", "b.bin"), 2048)
	mustWrite(t, filepath.Join(root, "sub", "deep", "c.bin"), 52)

	got, err := duDir(context.Background(), root)
	if err != nil {
		t.Fatalf("duDir: %v", err)
	}
	if got != 3100 {
		t.Fatalf("duDir = %d, want 3100", got)
	}
}

// TestDuDir_MissingRoot returns an error (volume dir gone) rather than 0-OK.
func TestDuDir_MissingRoot(t *testing.T) {
	if _, err := duDir(context.Background(), filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing root")
	}
}

// TestManagedDriverUsage exercises the capability through the Driver surface.
func TestManagedDriverUsage(t *testing.T) {
	root := t.TempDir()
	volDir := filepath.Join(root, "vol-1")
	mustWrite(t, filepath.Join(volDir, "data.db"), 4096)

	d, err := driver.New(DriverNameLocal, map[string]any{"localVolumeRoot": root})
	if err != nil {
		t.Fatalf("driver.New: %v", err)
	}
	ur, ok := d.(driver.UsageReporter)
	if !ok {
		t.Fatal("local driver must implement UsageReporter")
	}
	used, capacity, err := ur.Usage(context.Background(), driver.OpContext{}, driver.VolumeHandle(volDir))
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if used != 4096 {
		t.Fatalf("used = %d, want 4096", used)
	}
	if capacity != 0 {
		t.Fatalf("capacity = %d, want 0 (unknown for dir-backed volumes)", capacity)
	}
}

func mustWrite(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}
