package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// applyFromFileFlags reads `key=path` entries and pushes the file's
// full byte contents (newlines and all) into data[key]. This is the
// shape `rune create secret --from-file=tls.crt=cert.pem` resolves
// through and is the load-bearing fix for the multi-line truncation
// bug filed against dev.61.
func TestApplyFromFileFlags_KeyPathPreservesMultilineContent(t *testing.T) {
	dir := t.TempDir()
	pemPath := filepath.Join(dir, "cert.pem")
	pem := "-----BEGIN CERTIFICATE-----\nLINE1\nLINE2\nLINE3\n-----END CERTIFICATE-----\n"
	require.NoError(t, os.WriteFile(pemPath, []byte(pem), 0o600))

	data := map[string]string{}
	require.NoError(t, applyFromFileFlags([]string{"tls.crt=" + pemPath}, data))

	assert.Equal(t, pem, data["tls.crt"], "multi-line PEM must round-trip byte-for-byte")
	assert.Equal(t, 5, strings.Count(data["tls.crt"], "\n"), "all five newlines must survive")
}

// Two keys, two files, one call — covers the canonical
// --from-file=tls.crt=... --from-file=tls.key=... pattern.
func TestApplyFromFileFlags_MultipleKeyPathsCoexist(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath, []byte("cert-bytes\nwith-newline\n"), 0o600))
	require.NoError(t, os.WriteFile(keyPath, []byte("key-bytes\nwith-newline\n"), 0o600))

	data := map[string]string{}
	require.NoError(t, applyFromFileFlags(
		[]string{"tls.crt=" + certPath, "tls.key=" + keyPath},
		data,
	))

	assert.Equal(t, "cert-bytes\nwith-newline\n", data["tls.crt"])
	assert.Equal(t, "key-bytes\nwith-newline\n", data["tls.key"])
}

// --from-file=path (no key=) is rejected — every spec must name the
// destination key explicitly. Eliminates the ambiguity that bit the
// dev.61 ops debugging session.
func TestApplyFromFileFlags_PathWithoutKeyRejected(t *testing.T) {
	data := map[string]string{}
	err := applyFromFileFlags([]string{"./some-file"}, data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected key=path")
}

// --from-file=key= (empty path after '=') surfaces a clear error
// rather than silently writing an empty value.
func TestApplyFromFileFlags_EmptyPathRejected(t *testing.T) {
	data := map[string]string{}
	err := applyFromFileFlags([]string{"tls.crt="}, data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty path")
}
