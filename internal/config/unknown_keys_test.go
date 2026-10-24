package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeRunefile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestLoadWithUnknownKeys_ListsMisspelledKeys(t *testing.T) {
	p := writeRunefile(t, "runefile.toml", `
[server]
grpc_address = ":7863"
tls_enabeld = true

[secret.encryption]
enabeld = true

[docker]
secret_dir_mode = 448
`)
	cfg, unknown, err := LoadWithUnknownKeys(p)
	require.NoError(t, err)
	assert.Equal(t, ":7863", cfg.Server.GRPCAddr)
	// Keys read through the global viper (docker.*_mode) are not unknown.
	assert.Equal(t, []string{"secret.encryption.enabeld", "server.tls_enabeld"}, unknown)
}

func TestLoadWithUnknownKeys_NoneForAValidFile(t *testing.T) {
	_, unknown, err := LoadWithUnknownKeys("../../examples/runefile.toml")
	require.NoError(t, err)
	assert.Empty(t, unknown)

	// Keys with no Config field that other code reads.
	p := writeRunefile(t, "runefile.toml", `
debug = true
pretty = true

[docker]
config_file_mode = 420
disable_ambient_registry_auth = true

[[docker.registries]]
name = "ghcr"
registry = "ghcr.io"
[docker.registries.auth]
from_secret = "ghcr-creds"
`)
	_, unknown, err = LoadWithUnknownKeys(p)
	require.NoError(t, err)
	assert.Empty(t, unknown)
}

// A value of the wrong type used to come back as (nil, err), which startup
// dropped and then dereferenced.
func TestLoad_DecodeErrorNamesTheFile(t *testing.T) {
	p := writeRunefile(t, "runefile.toml", "[secret]\nencryption = \"on\"\n")
	cfg, err := Load(p)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), p)
}

// Startup also reads the runefile through the global viper, which exits on a
// syntax error first. Load must not depend on that ordering.
func TestLoad_SyntaxErrorIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"runefile.toml": "[server]\nrequire_tls = true\n[server\n",
		"runefile.yaml": "server:\n  require_tls: true\nlog:\n  level: info\n   bad: [x\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := writeRunefile(t, name, body)
			cfg, err := Load(p)
			require.Error(t, err)
			assert.Nil(t, cfg)
			assert.Contains(t, err.Error(), p)
		})
	}
}
