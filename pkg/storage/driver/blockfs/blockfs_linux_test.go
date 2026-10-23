//go:build linux

package blockfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Runs the real blkid and mkfs against loop devices, pinning the exit
// codes and output this package depends on.
func TestLinux_EnsureFormatted(t *testing.T) {
	requireLoop(t)
	ctx := context.Background()

	t.Run("populated disk is left alone", func(t *testing.T) {
		dev, img := loopImage(t)
		run(t, "mkfs.ext4", "-q", "-F", dev)
		mnt := t.TempDir()
		run(t, "mount", dev, mnt)
		require.NoError(t, os.WriteFile(filepath.Join(mnt, "data"), []byte("keep"), 0o644))
		run(t, "umount", mnt)

		require.NoError(t, EnsureFormatted(ctx, "test", dev, "ext4"))

		run(t, "mount", dev, mnt)
		defer func() { _ = exec.Command("umount", mnt).Run() }()
		b, err := os.ReadFile(filepath.Join(mnt, "data"))
		require.NoError(t, err, "the existing filesystem must survive (image %s)", img)
		assert.Equal(t, "keep", string(b))
	})

	t.Run("blank disk is formatted", func(t *testing.T) {
		dev, _ := loopImage(t)
		require.NoError(t, EnsureFormatted(ctx, "test", dev, "ext4"))
		assert.Equal(t, "ext4", run(t, "blkid", "-p", "-o", "value", "-s", "TYPE", dev))
	})

	t.Run("partitioned disk is refused", func(t *testing.T) {
		dev, _ := loopImage(t)
		if _, err := exec.LookPath("sfdisk"); err != nil {
			t.Skip("needs sfdisk")
		}
		cmd := exec.Command("sfdisk", "-q", dev)
		cmd.Stdin = strings.NewReader("label: gpt\n")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		err = EnsureFormatted(ctx, "test", dev, "ext4")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "partition table")
	})

	t.Run("missing device is an error, not blank", func(t *testing.T) {
		err := EnsureFormatted(ctx, "test", "/dev/disk/by-id/does-not-exist", "ext4")
		require.Error(t, err)
	})

	t.Run("cancelled context is an error, not blank", func(t *testing.T) {
		dev, _ := loopImage(t)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		require.Error(t, EnsureFormatted(cctx, "test", dev, "ext4"))
		out, _ := exec.Command("blkid", "-p", dev).CombinedOutput()
		assert.NotContains(t, string(out), "TYPE=", "nothing may have been formatted")
	})
}

func loopImage(t *testing.T) (dev, img string) {
	t.Helper()
	img = filepath.Join(t.TempDir(), "disk.img")
	f, err := os.Create(img)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(32<<20))
	require.NoError(t, f.Close())
	dev = run(t, "losetup", "--find", "--show", img)
	t.Cleanup(func() { _ = exec.Command("losetup", "-d", dev).Run() })
	return dev, img
}

// requireLoop skips unless loop devices work here. In the privileged CI
// job RUNE_REQUIRE_PRIVILEGED_MOUNT turns the skip into a failure.
func requireLoop(t *testing.T) {
	t.Helper()
	skip := t.Skipf
	if os.Getenv("RUNE_REQUIRE_PRIVILEGED_MOUNT") != "" {
		skip = t.Fatalf
	}
	if unix.Geteuid() != 0 {
		skip("needs root for loop devices")
	}
	for _, bin := range []string{"mkfs.ext4", "losetup", "blkid"} {
		if _, err := exec.LookPath(bin); err != nil {
			skip("needs %s", bin)
		}
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	require.NoError(t, err, "%s %s: %s", name, strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}
