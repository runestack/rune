package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_StorageDrivers verifies the runefile [storage.drivers]
// table is round-tripped into Config.Storage.Drivers as a per-driver
// opaque map[string]any.
func TestLoad_StorageDrivers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runefile.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
storage:
  drivers:
    local:
      localVolumeRoot: /var/lib/rune/volumes
    local-host:
      hostPathAllowlist:
        - /srv/rune
        - /mnt/data
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Storage.Drivers)

	local, ok := cfg.Storage.Drivers["local"]
	require.True(t, ok, "local driver block missing")
	// viper lowercases nested map keys; the local driver looks them
	// up case-insensitively so this is fine downstream.
	assert.Equal(t, "/var/lib/rune/volumes", local["localvolumeroot"])

	host, ok := cfg.Storage.Drivers["local-host"]
	require.True(t, ok, "local-host driver block missing")
	allow, ok := host["hostpathallowlist"].([]any)
	require.True(t, ok, "hostPathAllowlist should decode to []any")
	assert.ElementsMatch(t, []any{"/srv/rune", "/mnt/data"}, allow)
}

// TestLoad_StorageMissing verifies the absence of a storage block
// produces a nil Drivers map (orchestrator falls back to driver
// defaults).
func TestLoad_StorageMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runefile.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
data_dir: /tmp/rune
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Nil(t, cfg.Storage.Drivers)
	assert.Nil(t, cfg.Storage.DefaultStorageClass)
	assert.False(t, cfg.Storage.PreserveOnDelete)
	assert.False(t, cfg.Storage.AllowCreateMissing)
}

// TestLoad_StorageTypedKnobs verifies the typed [storage] knobs
// (defaultStorageClass, preserveOnDelete, allowCreateMissing) are
// round-tripped onto Config.Storage. *string distinguishes the
// "explicitly empty" case from "unset".
func TestLoad_StorageTypedKnobs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runefile.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
storage:
  defaultStorageClass: do-block-ssd
  preserveOnDelete: true
  allowCreateMissing: true
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Storage.DefaultStorageClass)
	assert.Equal(t, "do-block-ssd", *cfg.Storage.DefaultStorageClass)
	assert.True(t, cfg.Storage.PreserveOnDelete)
	assert.True(t, cfg.Storage.AllowCreateMissing)
}

// TestLoad_StorageDefaultStorageClassEmpty verifies the explicit
// empty-string case is preserved (vs being elided to nil).
func TestLoad_StorageDefaultStorageClassEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runefile.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
storage:
  defaultStorageClass: ""
`), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Storage.DefaultStorageClass, "empty string should be preserved as *string, not nil")
	assert.Equal(t, "", *cfg.Storage.DefaultStorageClass)
}
