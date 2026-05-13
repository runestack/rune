package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// StorageClass file format is the wrapped shape that matches `rune cast`
// (a top-level `storageClass:` key). StorageClass is cluster-scoped, so
// it has its own dedicated `rune storageclass create -f` command rather
// than routing through cast. The shape is identical to the cast-file
// shape so the same YAML works either way.

func TestReadStorageClassFile_WrappedShape(t *testing.T) {
	body := []byte(`
storageClass:
  name: do-lon1
  driver: do-volume
  parameters:
    region: lon1
    fsType: ext4
  reclaimPolicy: retain
`)
	sc, err := readStorageClassFile(writeStorageClassTempFile(t, "sc.yaml", body))
	require.NoError(t, err)
	assert.Equal(t, "do-lon1", sc.Name)
	assert.Equal(t, "do-volume", sc.Driver)
	assert.Equal(t, "lon1", sc.Parameters["region"])
}

// JSON is a subset of YAML — the unmarshaller treats it the same.
func TestReadStorageClassFile_WrappedJSON(t *testing.T) {
	body := []byte(`{"storageClass": {"name": "do-lon1", "driver": "do-volume"}}`)
	sc, err := readStorageClassFile(writeStorageClassTempFile(t, "sc.json", body))
	require.NoError(t, err)
	assert.Equal(t, "do-lon1", sc.Name)
}

// A flat-shape file (top-level `name:`/`driver:` instead of the
// `storageClass:` wrapper) is rejected with a pointer at the right
// shape rather than a generic "name is required".
func TestReadStorageClassFile_RejectsLegacyFlatShape(t *testing.T) {
	body := []byte(`
name: do-lon1
driver: do-volume
parameters:
  region: lon1
`)
	_, err := readStorageClassFile(writeStorageClassTempFile(t, "sc.yaml", body))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "top-level `storageClass:` key")
}

func TestReadStorageClassFile_MissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "wrapped without name",
			body: "storageClass:\n  driver: do-volume",
			want: "storage class name is required",
		},
		{
			name: "wrapped without driver",
			body: "storageClass:\n  name: do-lon1",
			want: "storage class driver is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readStorageClassFile(writeStorageClassTempFile(t, "sc.yaml", []byte(tc.body)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func writeStorageClassTempFile(t *testing.T, name string, body []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, body, 0o600))
	return p
}
