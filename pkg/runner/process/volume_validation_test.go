package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/require"
)

func TestValidateVolumeMounts_OK(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	err := r.validateVolumeMounts([]types.ResolvedVolumeMount{
		{Name: "data", MountPath: "/data", Source: dir, VolumeName: "data"},
	})
	require.NoError(t, err)
}

func TestValidateVolumeMounts_ReadOnlySkipsWriteProbe(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses 0o500 directory permissions")
	}
	r := newTestRunner(t)
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := r.validateVolumeMounts([]types.ResolvedVolumeMount{
		{Name: "data", MountPath: "/data", Source: dir, VolumeName: "data", ReadOnly: true},
	})
	require.NoError(t, err)
}

func TestValidateVolumeMounts_MissingSource(t *testing.T) {
	r := newTestRunner(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err := r.validateVolumeMounts([]types.ResolvedVolumeMount{
		{Name: "data", MountPath: "/data", Source: missing, VolumeName: "data"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not exist")
}

func TestValidateVolumeMounts_NotADirectory(t *testing.T) {
	r := newTestRunner(t)
	f := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0o600))
	err := r.validateVolumeMounts([]types.ResolvedVolumeMount{
		{Name: "data", MountPath: "/data", Source: f, VolumeName: "data"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a directory")
}

func TestValidateVolumeMounts_NotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses 0o500 directory permissions")
	}
	r := newTestRunner(t)
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := r.validateVolumeMounts([]types.ResolvedVolumeMount{
		{Name: "data", MountPath: "/data", Source: dir, VolumeName: "data"},
	})
	require.Error(t, err)
	if !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("expected 'not writable' error, got: %v", err)
	}
}

func TestValidateVolumeMounts_EmptySource(t *testing.T) {
	r := newTestRunner(t)
	err := r.validateVolumeMounts([]types.ResolvedVolumeMount{
		{Name: "data", MountPath: "/data", Source: "", VolumeName: "data"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty source path")
}
