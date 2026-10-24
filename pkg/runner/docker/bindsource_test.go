package docker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A container controls everything inside its volume, including making a
// subPath component a symlink out of it.
func TestBindSource(t *testing.T) {
	vol := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(vol, "sub", "deep"), 0o750))
	require.NoError(t, os.Symlink(outside, filepath.Join(vol, "escape")))
	require.NoError(t, os.Symlink("/", filepath.Join(vol, "sub", "root")))
	require.NoError(t, os.Symlink(filepath.Join(vol, "sub"), filepath.Join(vol, "inner")))

	for _, sub := range []string{"escape", "sub/root/etc", "sub/root", "../" + filepath.Base(outside)} {
		_, err := bindSource(vol, sub)
		assert.Error(t, err, sub)
	}

	got, err := bindSource(vol, "sub/deep")
	require.NoError(t, err)
	want, _ := filepath.EvalSymlinks(filepath.Join(vol, "sub", "deep"))
	assert.Equal(t, want, got)

	got, err = bindSource(vol, "inner")
	require.NoError(t, err, "a symlink that stays inside the volume is fine")
	want, _ = filepath.EvalSymlinks(filepath.Join(vol, "sub"))
	assert.Equal(t, want, got)

	got, err = bindSource(vol, "")
	require.NoError(t, err)
	assert.Equal(t, vol, got, "no subPath binds the source as given")
}
