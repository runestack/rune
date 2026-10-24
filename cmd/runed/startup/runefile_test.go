package startup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/runestack/rune/pkg/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadRunefile_UnknownKeysAreOneError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "runefile.toml")
	require.NoError(t, os.WriteFile(p, []byte("[server]\ntls_enabeld = true\n[auth]\nallow_remote_admn = true\n"), 0o600))
	logger := log.NewTestLogger()

	cfg, err := loadRunefile(logger, p)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	var errs []log.TestEntry
	for _, e := range logger.GetEntries() {
		if e.Level == log.ErrorLevel {
			errs = append(errs, e)
		}
	}
	require.Len(t, errs, 1)
	fields := map[string]any{}
	for _, f := range errs[0].Fields {
		fields[f.Key] = f.Value
	}
	assert.Equal(t, p, fields["path"])
	assert.Equal(t, "auth.allow_remote_admn, server.tls_enabeld", fields["keys"])
}

func TestLoadRunefile_DecodeErrorIsReturned(t *testing.T) {
	p := filepath.Join(t.TempDir(), "runefile.toml")
	require.NoError(t, os.WriteFile(p, []byte("[secret]\nencryption = \"on\"\n"), 0o600))

	cfg, err := loadRunefile(log.NewTestLogger(), p)
	require.Error(t, err)
	assert.Nil(t, cfg)
}
