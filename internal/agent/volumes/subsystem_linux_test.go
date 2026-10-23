//go:build linux

package volumes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/storage/driver/mountsync"
	"github.com/runestack/rune/pkg/storage/mountinfo"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loopDriver backs each volume with an ext4 image on a loop device. Its
// handle is the image path. Mount and Unmount behave like the cloud
// drivers': idempotent by what the mount table shows, with the
// production flush-then-unmount.
type loopDriver struct {
	fakeDriver
}

func (d *loopDriver) Attach(_ context.Context, _ driver.OpContext, h driver.VolumeHandle, _ driver.NodeID) (driver.DevicePath, error) {
	if dev := loopOf(string(h)); dev != "" {
		return driver.DevicePath(dev), nil
	}
	out, err := exec.Command("losetup", "--find", "--show", string(h)).Output()
	if err != nil {
		return "", err
	}
	return driver.DevicePath(strings.TrimSpace(string(out))), nil
}

func (d *loopDriver) Detach(_ context.Context, _ driver.OpContext, h driver.VolumeHandle, node driver.NodeID) error {
	d.mu.Lock()
	d.detachCalls = append(d.detachCalls, node)
	d.mu.Unlock()
	if dev := loopOf(string(h)); dev != "" {
		return exec.Command("losetup", "-d", dev).Run()
	}
	return nil
}

func (d *loopDriver) Mount(_ context.Context, _ driver.OpContext, o driver.MountOpts) (driver.MountTarget, error) {
	own, err := mountinfo.DeviceOf(string(o.Device))
	if err != nil {
		return "", err
	}
	if stack, _ := mountinfo.Stack(string(o.Target)); len(stack) > 0 && stack[len(stack)-1] == own {
		return o.Target, nil
	}
	if err := os.MkdirAll(string(o.Target), 0o750); err != nil {
		return "", err
	}
	return o.Target, unix.Mount(string(o.Device), string(o.Target), "ext4", 0, "")
}

func (d *loopDriver) Unmount(_ context.Context, _ driver.OpContext, target driver.MountTarget) error {
	d.mu.Lock()
	d.unmountCalls = append(d.unmountCalls, target)
	d.mu.Unlock()
	return mountsync.Unmount("loop", string(target))
}

// loopOf returns the loop device backing img, or "".
func loopOf(img string) string {
	out, err := exec.Command("losetup", "-j", img).Output()
	if err != nil {
		return ""
	}
	dev, _, _ := strings.Cut(strings.TrimSpace(string(out)), ":")
	return dev
}

// Replays the upgrade of a node hit by the namespace collision, on real
// loop-backed ext4: trial's disk mounted over prod's at the old flat
// path. Each volume must come up on its own disk, prod's buried disk must
// not be detached while still mounted, and once trial's disk goes the
// retry tick must clean prod's up.
func TestLinux_UpgradeWithStackedOldPath(t *testing.T) {
	requireLoop(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "mounts")
	const id = "mongodb-data-mongodb-0"
	legacy := filepath.Join(root, id)
	require.NoError(t, os.MkdirAll(legacy, 0o750))

	imgs := map[string]string{}
	prodCtr := ""
	for _, ns := range []string{"prod", "trial"} {
		img := filepath.Join(dir, ns+".img")
		makeImage(t, img)
		imgs[ns] = img
		dev := run(t, "losetup", "--find", "--show", img)
		t.Cleanup(func() { _ = exec.Command("losetup", "-d", dev).Run() })
		run(t, "mount", dev, legacy)
		require.NoError(t, os.WriteFile(filepath.Join(legacy, "owner"), []byte(ns), 0o644))
		if ns == "prod" && dockerOK(t) {
			// A prod container started before trial's disk was mounted
			// over the path: its bind holds prod's disk.
			prodCtr = startContainer(t, legacy)
		}
	}
	t.Cleanup(func() {
		for i := 0; i < 4; i++ {
			_ = exec.Command("umount", "-l", legacy).Run()
		}
		_ = exec.Command("umount", "-l", filepath.Join(root, "prod", id)).Run()
		_ = exec.Command("umount", "-l", filepath.Join(root, "trial", id)).Run()
	})

	drv := &loopDriver{fakeDriver{name: "fake"}}
	sub, err := New(Config{
		Store:     store.NewTestStore(),
		NodeID:    "node-a",
		Lookup:    func(context.Context, *types.Volume) (driver.Driver, error) { return drv, nil },
		MountRoot: root,
		Logger:    log.NewLogger(),
	})
	require.NoError(t, err)
	ctx := context.Background()
	vols := map[string]*types.Volume{}
	for ns, img := range imgs {
		v := boundVolume(id, ns, id, "node-a")
		v.Handle = img
		vols[ns] = v
		require.NoError(t, sub.reconcile(ctx, v))
	}

	owner := func(ns string) string {
		target, ok := sub.MountTargetFor(vols[ns].MountKey())
		require.True(t, ok, ns)
		b, err := os.ReadFile(filepath.Join(target, "owner"))
		require.NoError(t, err)
		return string(b)
	}
	assert.Equal(t, "prod", owner("prod"), "prod must see its own disk")
	assert.Equal(t, "trial", owner("trial"), "trial must see its own disk")
	pt, _ := sub.MountTargetFor(vols["prod"].MountKey())
	tt, _ := sub.MountTargetFor(vols["trial"].MountKey())
	assert.Equal(t, filepath.Join(sub.cfg.MountRoot, "prod", id), pt)
	assert.Equal(t, filepath.Join(sub.cfg.MountRoot, id), tt, "trial's disk is on top of the old path, so it keeps it")
	var ctrs []string
	if prodCtr != "" {
		assert.Equal(t, "prod", ctrRead(t, prodCtr), "the running prod container still sees prod's disk")
		ctrs = append(ctrs, prodCtr, startContainer(t, pt), startContainer(t, tt))
		assert.Equal(t, "prod", ctrRead(t, ctrs[1]), "a new prod container gets prod's disk")
		assert.Equal(t, "trial", ctrRead(t, ctrs[2]), "a new trial container gets trial's disk")
	}

	// Unbind prod. Its disk is still buried at the old path: no detach.
	vols["prod"].BoundNode = "node-b"
	require.Error(t, sub.reconcile(ctx, vols["prod"]))
	assert.NotEmpty(t, loopOf(imgs["prod"]), "prod's disk must stay attached while still mounted")
	sub.retryParked(ctx)
	assert.NotEmpty(t, loopOf(imgs["prod"]))
	if prodCtr != "" {
		run(t, "docker", "exec", prodCtr, "sh", "-c", "echo after > /data/write && sync")
		assert.Equal(t, "prod", ctrRead(t, prodCtr), "the prod container keeps working while its disk stays attached")
	}
	// From here nothing may hold the disks. A container's bind lives in its
	// own mount namespace, which the detach guard cannot see; that gap is
	// #272.
	for _, c := range ctrs {
		run(t, "docker", "rm", "-f", c)
	}

	// Unbind trial; prod's disk surfaces at the old path. The next retry
	// unmounts it there and detaches it.
	vols["trial"].BoundNode = "node-b"
	require.NoError(t, sub.reconcile(ctx, vols["trial"]))
	assert.Empty(t, loopOf(imgs["trial"]))
	sub.retryParked(ctx)
	assert.Empty(t, loopOf(imgs["prod"]), "prod's disk is detached once nothing holds it")
	stack, err := mountinfo.Stack(filepath.Join(sub.cfg.MountRoot, id))
	require.NoError(t, err)
	assert.Empty(t, stack)
}

// The common upgrade case: one disk alone on its old path keeps that
// path, and a container bound there before the upgrade keeps its data.
func TestLinux_UpgradeWithSingleOldPath(t *testing.T) {
	requireLoop(t)
	dir := t.TempDir()
	root := filepath.Join(dir, "mounts")
	const id = "data-db-0"
	legacy := filepath.Join(root, id)
	require.NoError(t, os.MkdirAll(legacy, 0o750))
	img := filepath.Join(dir, "prod.img")
	makeImage(t, img)
	dev := run(t, "losetup", "--find", "--show", img)
	t.Cleanup(func() { _ = exec.Command("losetup", "-d", dev).Run() })
	run(t, "mount", dev, legacy)
	t.Cleanup(func() { _ = exec.Command("umount", "-l", legacy).Run() })
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "owner"), []byte("prod"), 0o644))
	ctr := ""
	if dockerOK(t) {
		ctr = startContainer(t, legacy)
	}

	drv := &loopDriver{fakeDriver{name: "fake"}}
	sub, err := New(Config{
		Store:     store.NewTestStore(),
		NodeID:    "node-a",
		Lookup:    func(context.Context, *types.Volume) (driver.Driver, error) { return drv, nil },
		MountRoot: root,
		Logger:    log.NewLogger(),
	})
	require.NoError(t, err)
	v := boundVolume(id, "prod", id, "node-a")
	v.Handle = img
	require.NoError(t, sub.reconcile(context.Background(), v))

	target, ok := sub.MountTargetFor(v.MountKey())
	require.True(t, ok)
	assert.Equal(t, filepath.Join(sub.cfg.MountRoot, id), target)
	stack, err := mountinfo.Stack(target)
	require.NoError(t, err)
	assert.Len(t, stack, 1, "adoption must not mount the disk a second time")
	if ctr != "" {
		assert.Equal(t, "prod", ctrRead(t, ctr))
	}
}

// dockerOK reports whether a Docker daemon is usable. RUNE_REQUIRE_DOCKER
// makes its absence a failure.
func dockerOK(t *testing.T) bool {
	t.Helper()
	if exec.Command("docker", "info").Run() == nil {
		return true
	}
	if os.Getenv("RUNE_REQUIRE_DOCKER") != "" {
		t.Fatal("RUNE_REQUIRE_DOCKER is set but docker is not usable")
	}
	return false
}

// startContainer runs a container that binds src at /data, as the Docker
// runner does for a volume.
func startContainer(t *testing.T, src string) string {
	t.Helper()
	id := run(t, "docker", "run", "-d", "--rm", "-v", src+":/data", "alpine:latest", "sleep", "600")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	return id
}

func ctrRead(t *testing.T, ctr string) string {
	t.Helper()
	return run(t, "docker", "exec", ctr, "cat", "/data/owner")
}

// requireLoop skips unless loop mounts work here. In the privileged CI
// job RUNE_REQUIRE_PRIVILEGED_MOUNT turns the skip into a failure, so a
// degraded runner cannot go green by skipping.
func requireLoop(t *testing.T) {
	t.Helper()
	skip := t.Skipf
	if os.Getenv("RUNE_REQUIRE_PRIVILEGED_MOUNT") != "" {
		skip = t.Fatalf
	}
	if unix.Geteuid() != 0 {
		skip("needs root for loop mounts")
	}
	for _, bin := range []string{"mkfs.ext4", "losetup"} {
		if _, err := exec.LookPath(bin); err != nil {
			skip("needs %s", bin)
		}
	}
}

func makeImage(t *testing.T, img string) {
	t.Helper()
	f, err := os.Create(img)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(32<<20))
	require.NoError(t, f.Close())
	run(t, "mkfs.ext4", "-q", "-F", img)
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	require.NoError(t, err, "%s %s: %s", name, strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}
