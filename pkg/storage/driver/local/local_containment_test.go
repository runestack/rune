package local_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/types"
)

func mustMkdir(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o750); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustSymlink(t *testing.T, target, link string) string {
	t.Helper()
	mustMkdir(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A local volume's handle becomes a container bind source. Only a
// directory inside the managed volume root may be mounted, however the
// handle tries to reach outside it.
func TestManagedMount_Containment(t *testing.T) {
	d, root := newManagedDriver(t)
	outside := t.TempDir()
	good := mustMkdir(t, filepath.Join(root, "prod", "data"))
	mustMkdir(t, filepath.Join(root, ".snapshots", "prod", "snap"))

	cases := map[string]string{
		"outside the root":           outside,
		"the root itself":            root,
		"dot-dot out of the root":    filepath.Join(root, "prod", "..", "..", filepath.Base(outside)),
		"symlink to outside":         mustSymlink(t, outside, filepath.Join(root, "prod", "escape")),
		"through a symlinked parent": filepath.Join(mustSymlink(t, outside, filepath.Join(root, "trial", "link")), "x"),
		"the snapshot store":         filepath.Join(root, ".snapshots", "prod", "snap"),
		"host root":                  "/",
	}
	mustMkdir(t, filepath.Join(outside, "x"))
	for name, h := range cases {
		if _, err := d.Mount(context.Background(), driver.OpContext{}, driver.MountOpts{Handle: driver.VolumeHandle(h)}); err == nil {
			t.Errorf("%s: Mount(%q) succeeded, want refusal", name, h)
		}
	}

	got, err := d.Mount(context.Background(), driver.OpContext{}, driver.MountOpts{Handle: driver.VolumeHandle(good)})
	if err != nil {
		t.Fatalf("Mount of a real volume dir: %v", err)
	}
	if string(got) != resolved(t, good) {
		t.Errorf("Mount returned %q, want the resolved path %q", got, resolved(t, good))
	}
}

// Delete must not follow a symlinked path component out of the root.
func TestManagedDelete_DoesNotFollowSymlinkOut(t *testing.T) {
	d, root := newManagedDriver(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "x")
	mustMkdir(t, filepath.Join(victim, "keep"))
	link := mustSymlink(t, outside, filepath.Join(root, "prod", "link"))

	if err := d.Delete(context.Background(), driver.OpContext{}, driver.VolumeHandle(filepath.Join(link, "x"))); err == nil {
		t.Error("Delete through a symlink out of the root succeeded")
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Errorf("data outside the root was removed: %v", err)
	}
}

// Snapshot copies from the handle and restore copies from the snapshot;
// neither may read outside its own store.
func TestManagedSnapshotRestore_Containment(t *testing.T) {
	d, root := newManagedDriver(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := &types.Snapshot{Name: "s", Namespace: "prod"}
	if _, err := d.Snapshot(context.Background(), driver.OpContext{}, driver.SnapshotRequest{Handle: driver.VolumeHandle(outside), Snapshot: snap}); err == nil {
		t.Error("Snapshot of a handle outside the root succeeded")
	}
	vol := &types.Volume{Name: "restored", Namespace: "prod"}
	if _, err := d.RestoreFromSnapshot(context.Background(), driver.OpContext{Volume: vol}, driver.RestoreRequest{SourceHandle: driver.SnapshotHandle(outside)}); err == nil {
		t.Error("Restore from a source outside the snapshot store succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, "prod", "restored", "secret")); err == nil {
		t.Error("restore copied data from outside the snapshot store")
	}
}

// local-host binds operator paths, but only allowlisted ones, and the
// handle that is bound is checked, not just the path seen at provision.
func TestHostMount_Allowlist(t *testing.T) {
	allowed := t.TempDir()
	d := newHostDriver(t, allowed)
	outside := t.TempDir()

	for name, h := range map[string]string{
		"outside the allowlist": outside,
		"symlink out of it":     mustSymlink(t, outside, filepath.Join(allowed, "escape")),
	} {
		if _, err := d.Mount(context.Background(), driver.OpContext{}, driver.MountOpts{Handle: driver.VolumeHandle(h)}); err == nil {
			t.Errorf("%s: Mount(%q) succeeded, want refusal", name, h)
		}
	}
	good := mustMkdir(t, filepath.Join(allowed, "data"))
	if _, err := d.Mount(context.Background(), driver.OpContext{}, driver.MountOpts{Handle: driver.VolumeHandle(good)}); err != nil {
		t.Errorf("Mount of an allowlisted dir: %v", err)
	}
}

// An allowlist entry reached through a symlink still admits its own
// directories.
func TestHostMount_SymlinkedAllowlistEntry(t *testing.T) {
	real := t.TempDir()
	alias := mustSymlink(t, real, filepath.Join(t.TempDir(), "alias"))
	d := newHostDriver(t, alias)
	good := mustMkdir(t, filepath.Join(real, "data"))
	if _, err := d.Mount(context.Background(), driver.OpContext{}, driver.MountOpts{Handle: driver.VolumeHandle(filepath.Join(alias, "data"))}); err != nil {
		t.Errorf("Mount via a symlinked allowlist entry: %v", err)
	}
	_ = good
}

// Usage walks the handle; outside the driver's own paths it would size
// arbitrary host directories.
func TestUsage_Containment(t *testing.T) {
	m, _ := newManagedDriver(t)
	allowed := t.TempDir()
	h := newHostDriver(t, allowed)
	outside := t.TempDir()
	for name, d := range map[string]driver.Driver{"local": m, "local-host": h} {
		ur := d.(driver.UsageReporter)
		if _, _, err := ur.Usage(context.Background(), driver.OpContext{}, driver.VolumeHandle(outside)); err == nil {
			t.Errorf("%s: Usage outside its paths succeeded", name)
		}
	}
}
