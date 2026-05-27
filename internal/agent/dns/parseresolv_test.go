package dns

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeResolvConf returns a path to a temp resolv.conf containing the
// supplied lines verbatim.
func writeResolvConf(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "resolv.conf")
	require.NoError(t, os.WriteFile(p, []byte(joinLines(lines)), 0o644))
	return p
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

// Regression: on stock Ubuntu /etc/resolv.conf only lists
// 127.0.0.53 (the systemd-resolved stub). When the agent's own bind
// IP is 127.0.0.123, parseResolvConf must keep 127.0.0.53 — otherwise
// the agent has no upstreams and every external DNS query returns
// SERVFAIL. See user incident around api.resend.com.
func TestParseResolvConf_KeepsSystemdResolvedStub(t *testing.T) {
	path := writeResolvConf(t, "nameserver 127.0.0.53")

	got, err := parseResolvConf(path, bindIPSet([]string{"127.0.0.123:53"}))
	require.NoError(t, err)
	assert.Equal(t, []string{"127.0.0.53:53"}, got,
		"systemd-resolved must NOT be skipped when it isn't the agent's own bind")
}

// Conversely, an upstream entry that exactly matches one of the
// agent's bind IPs must be skipped — forwarding to ourselves loops.
func TestParseResolvConf_SkipsOwnBindIP(t *testing.T) {
	path := writeResolvConf(t,
		"nameserver 127.0.0.123",
		"nameserver 8.8.8.8",
	)

	got, err := parseResolvConf(path, bindIPSet([]string{"127.0.0.123:53"}))
	require.NoError(t, err)
	assert.Equal(t, []string{"8.8.8.8:53"}, got)
}

// Multiple agent bind addresses are all skipped; non-loopback
// nameservers and other loopback resolvers pass through.
func TestParseResolvConf_MultipleBinds(t *testing.T) {
	path := writeResolvConf(t,
		"nameserver 127.0.0.123",
		"nameserver 127.0.0.1",
		"nameserver 127.0.0.53",
		"nameserver 1.1.1.1",
	)
	skip := bindIPSet([]string{"127.0.0.123:53", "127.0.0.1:15353"})
	got, err := parseResolvConf(path, skip)
	require.NoError(t, err)
	assert.Equal(t, []string{"127.0.0.53:53", "1.1.1.1:53"}, got)
}

// An empty skip set keeps every entry verbatim.
func TestParseResolvConf_EmptySkipKeepsAll(t *testing.T) {
	path := writeResolvConf(t, "nameserver 127.0.0.53", "nameserver 8.8.4.4")
	got, err := parseResolvConf(path, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"127.0.0.53:53", "8.8.4.4:53"}, got)
}

func TestBindIPSet_ExtractsHostFromHostPort(t *testing.T) {
	got := bindIPSet([]string{"127.0.0.123:53", "10.0.0.5:53", "not-an-ip:53"})
	assert.Contains(t, got, "127.0.0.123")
	assert.Contains(t, got, "10.0.0.5")
	assert.NotContains(t, got, "not-an-ip", "non-IPs are silently dropped")
}
