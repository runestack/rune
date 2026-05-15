package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plain (non-runeset) cast files now accept --values and --set so
// operators can parameterise a single file from CI without converting
// to a full runeset.
func TestParseCastFilesResources_RendersValuesOnPlainCastFile(t *testing.T) {
	dir := t.TempDir()
	castPath := filepath.Join(dir, "svc.yaml")
	valuesPath := filepath.Join(dir, "values.yaml")

	// Cast file references {{ values.image }} and {{ values.scale }}.
	require.NoError(t, os.WriteFile(castPath, []byte(`
service:
  name: api
  namespace: default
  image: "{{ values:image }}"
  scale: 1
  ports:
    - name: http
      port: 80
`), 0o600))
	require.NoError(t, os.WriteFile(valuesPath, []byte(`
image: nginx:1.27.3
`), 0o600))

	opts := &castOptions{
		valuesFiles: []string{valuesPath},
	}
	opts.namespace = "default"
	values, err := mergeCastFileValues(opts)
	require.NoError(t, err)

	info, err := parseCastFilesResources([]string{castPath}, []string{castPath}, opts, values)
	require.NoError(t, err)

	require.Len(t, info.ServicesByFile[castPath], 1)
	svc := info.ServicesByFile[castPath][0]
	assert.Equal(t, "api", svc.Name)
	assert.Equal(t, "nginx:1.27.3", svc.Image, "values.image must be substituted before parsing")
}

// --set overrides values from --values files and uses dotted-path syntax.
func TestMergeCastFileValues_SetOverridesValuesFile(t *testing.T) {
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(valuesPath, []byte("image: nginx:1.27.3\n"), 0o600))

	opts := &castOptions{
		valuesFiles: []string{valuesPath},
		setValues:   []string{"image=nginx:edge"},
	}
	values, err := mergeCastFileValues(opts)
	require.NoError(t, err)
	assert.Equal(t, "nginx:edge", values["image"])
}

// Without --values / --set, the merge helper returns an empty map
// and the cast file parses verbatim (no template engine cost).
func TestMergeCastFileValues_EmptyWhenNoSources(t *testing.T) {
	values, err := mergeCastFileValues(&castOptions{})
	require.NoError(t, err)
	assert.Empty(t, values)
}
