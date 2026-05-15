package acme

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testKEK is a fixed 32-byte key used to satisfy the SecretRepo's KEK
// requirement in tests. The MemoryStore doesn't carry KEK config the
// way the BadgerStore does, so we inject one here.
var testKEK = []byte("0123456789abcdef0123456789abcdef")

func newTestCertStore(t *testing.T) *BadgerCertStore {
	t.Helper()
	ts := store.NewMemoryStore()
	require.NoError(t, ts.Open(""))
	t.Cleanup(func() { _ = ts.Close() })
	return NewBadgerCertStore(ts, repos.WithKEKBytes(testKEK))
}

// Round-trip Set → Get on a clean store: cert + key come back byte-for-byte.
// This is the load-bearing property — without it, runed restarts can't reuse
// a previously-issued cert and would re-hit the ACME provider, tripping LE's
// per-identifier-set rate limit.
func TestBadgerCertStore_SetGetRoundTrip(t *testing.T) {
	st := newTestCertStore(t)

	cert := []byte("-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n")
	key := []byte("-----BEGIN PRIVATE KEY-----\nFAKE\n-----END PRIVATE KEY-----\n")
	require.NoError(t, st.Set(context.Background(), "docs.example.com", cert, key))

	gotCert, gotKey, err := st.Get(context.Background(), "docs.example.com")
	require.NoError(t, err)
	assert.Equal(t, cert, gotCert)
	assert.Equal(t, key, gotKey)
}

// A missing host returns (nil, nil, nil) — the sentinel the orchestrator's
// check-before-issue gate consumes. Returning an error here would make every
// fresh-cluster boot look like a failure.
func TestBadgerCertStore_GetMissingHostReturnsNil(t *testing.T) {
	st := newTestCertStore(t)

	cert, key, err := st.Get(context.Background(), "nope.example.com")
	require.NoError(t, err)
	assert.Nil(t, cert)
	assert.Nil(t, key)
}

// Re-Set on an existing host overwrites in place. Locks in the "renewal
// replaces the row, doesn't accumulate them" property.
func TestBadgerCertStore_SetIsUpsert(t *testing.T) {
	st := newTestCertStore(t)

	require.NoError(t, st.Set(context.Background(), "h.example.com", []byte("c1"), []byte("k1")))
	require.NoError(t, st.Set(context.Background(), "h.example.com", []byte("c2"), []byte("k2")))

	cert, key, err := st.Get(context.Background(), "h.example.com")
	require.NoError(t, err)
	assert.Equal(t, []byte("c2"), cert)
	assert.Equal(t, []byte("k2"), key)
}

// Delete is idempotent on a missing host so the orchestrator's lifecycle
// cleanup doesn't surface spurious errors.
func TestBadgerCertStore_DeleteIdempotent(t *testing.T) {
	st := newTestCertStore(t)

	require.NoError(t, st.Delete(context.Background(), "never-existed.example.com"))
}

// certNameFor must produce stable DNS-1123 names regardless of how exotic
// the host looks. Locks in (a) determinism (same host → same name) and
// (b) that the output never exceeds the 63-char DNS label cap.
func TestCertNameFor_StableAndDNS1123(t *testing.T) {
	cases := []string{
		"example.com",
		"docs.with.dots.example.com",
		"*.wildcard.example.com",
		"with-hyphens.example.com",
		"xn--punycode-example.com",
		"UPPERCASE.example.com",
	}
	for _, host := range cases {
		t.Run(host, func(t *testing.T) {
			a := certNameFor(host)
			b := certNameFor(host)
			assert.Equal(t, a, b, "same host must produce same name")
			assert.LessOrEqual(t, len(a), 63, "DNS-1123 label cap")
			for _, r := range a {
				ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
				assert.True(t, ok, "rune %q in %q must be DNS-1123", r, a)
			}
		})
	}
}
