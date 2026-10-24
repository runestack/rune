//go:build linux

package mountinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Reads the real table: "/" is always mounted.
func TestStack_Root(t *testing.T) {
	got, err := Stack("/")
	require.NoError(t, err)
	assert.NotEmpty(t, got)
}

func TestDeviceOf_NotBlock(t *testing.T) {
	_, err := DeviceOf("/proc/self/mountinfo")
	assert.ErrorContains(t, err, "not a block device")
}

func TestLinux_MountedAt(t *testing.T) {
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
	dir := t.TempDir()
	loop := func(name string) string {
		img := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(img, nil, 0o600))
		require.NoError(t, os.Truncate(img, 32<<20))
		out, err := exec.Command("mkfs.ext4", "-q", "-F", img).CombinedOutput()
		require.NoError(t, err, string(out))
		dev, err := exec.Command("losetup", "--find", "--show", img).Output()
		require.NoError(t, err)
		d := strings.TrimSpace(string(dev))
		t.Cleanup(func() { _ = exec.Command("losetup", "-d", d).Run() })
		return d
	}
	a, b := loop("a.img"), loop("b.img")
	target := filepath.Join(dir, "mnt")
	require.NoError(t, os.MkdirAll(target, 0o750))

	got, err := MountedAt(target, a)
	require.NoError(t, err)
	assert.False(t, got, "nothing mounted yet")

	require.NoError(t, unix.Mount(a, target, "ext4", 0, ""))
	t.Cleanup(func() { _ = unix.Unmount(target, unix.MNT_DETACH) })
	got, err = MountedAt(target, a)
	require.NoError(t, err)
	assert.True(t, got)

	_, err = MountedAt(target, b)
	assert.Error(t, err, "another disk at the target is an error, not \"mounted\"")

	_, err = MountedAt(target, filepath.Join(dir, "no-such-device"))
	assert.Error(t, err, "an unreadable device is an error, not \"not mounted\"")
}
