package driverparams

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLookup is a small SecretLookup that pretends one secret exists
// (with two fields) and rejects everything else. Keeps the tests
// focused on the resolver's parse-and-dispatch logic.
type fakeLookup struct {
	calls int
}

func (f *fakeLookup) lookup(_ context.Context, ns, name, key string) (string, error) {
	f.calls++
	if ns == "shared" && name == "do-api-token" {
		switch key {
		case "token":
			return "dop_v1_resolved", nil
		case "fingerprint":
			return "ab:cd:ef", nil
		}
	}
	return "", errors.New("driverparams test: secret not found")
}

func TestResolve_PassesThroughLiterals(t *testing.T) {
	params := map[string]string{
		"region":  "nyc3",
		"fsType":  "ext4",
		"comment": "this is just a string",
	}
	got, err := Resolve(context.Background(), params, "", nil)
	require.NoError(t, err)
	assert.Equal(t, params, got)
	// Returned map must be independent of the input so callers can
	// mutate either safely. (Empty input is allowed to return the same
	// reference per the docstring, but non-empty input gets a copy.)
	got["region"] = "MUTATED"
	assert.Equal(t, "nyc3", params["region"])
}

func TestResolve_ResolvesShorthandSecretRef(t *testing.T) {
	fl := &fakeLookup{}
	params := map[string]string{
		"apiToken": "secret:do-api-token/token",
		"region":   "nyc3",
	}
	// Minimal shorthand has no namespace; defaultNamespace fills it in.
	got, err := Resolve(context.Background(), params, "shared", fl.lookup)
	require.NoError(t, err)
	assert.Equal(t, "dop_v1_resolved", got["apiToken"])
	assert.Equal(t, "nyc3", got["region"])
	assert.Equal(t, 1, fl.calls, "exactly one secret lookup performed")
}

func TestResolve_ResolvesFQDNSecretRef(t *testing.T) {
	fl := &fakeLookup{}
	params := map[string]string{
		"apiToken": "secret:do-api-token.shared.rune/token",
	}
	// Namespace is embedded in the FQDN; no default needed.
	got, err := Resolve(context.Background(), params, "", fl.lookup)
	require.NoError(t, err)
	assert.Equal(t, "dop_v1_resolved", got["apiToken"])
}

func TestResolve_MultipleRefsResolvedIndependently(t *testing.T) {
	fl := &fakeLookup{}
	params := map[string]string{
		"apiToken":    "secret:do-api-token/token",
		"fingerprint": "secret:do-api-token/fingerprint",
	}
	got, err := Resolve(context.Background(), params, "shared", fl.lookup)
	require.NoError(t, err)
	assert.Equal(t, "dop_v1_resolved", got["apiToken"])
	assert.Equal(t, "ab:cd:ef", got["fingerprint"])
}

func TestResolve_NonSecretRefsPassThrough(t *testing.T) {
	// configmap refs aren't this resolver's concern — the driver may
	// have its own use for them. Pass through verbatim.
	params := map[string]string{
		"configRef": "configmap:app-settings/db",
	}
	got, err := Resolve(context.Background(), params, "", nil)
	require.NoError(t, err)
	assert.Equal(t, "configmap:app-settings/db", got["configRef"])
}

func TestResolve_SecretRefWithNilLookupErrors(t *testing.T) {
	params := map[string]string{
		"apiToken": "secret:do-api-token/token",
	}
	_, err := Resolve(context.Background(), params, "shared", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no SecretLookup is wired")
	assert.Contains(t, err.Error(), "apiToken")
}

func TestResolve_SecretRefWithoutNamespaceOrDefaultErrors(t *testing.T) {
	fl := &fakeLookup{}
	params := map[string]string{
		"apiToken": "secret:do-api-token/token",
	}
	_, err := Resolve(context.Background(), params, "", fl.lookup)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "omits namespace")
	assert.Equal(t, 0, fl.calls, "no lookup performed when namespace can't be resolved")
}

func TestResolve_SecretRefWithoutKeyErrors(t *testing.T) {
	fl := &fakeLookup{}
	params := map[string]string{
		"apiToken": "secret:do-api-token",
	}
	_, err := Resolve(context.Background(), params, "shared", fl.lookup)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "omits the '/<key>' suffix")
}

func TestResolve_LookupErrorPropagated(t *testing.T) {
	fl := &fakeLookup{}
	params := map[string]string{
		// Wrong name → lookup returns error.
		"apiToken": "secret:wrong-name/token",
	}
	_, err := Resolve(context.Background(), params, "shared", fl.lookup)
	require.Error(t, err)
	// Wrap context: which parameter, which ref.
	assert.True(t, strings.Contains(err.Error(), "apiToken"),
		"error should name the offending parameter; got: %v", err)
	assert.True(t, strings.Contains(err.Error(), "wrong-name"),
		"error should include the ref; got: %v", err)
}

func TestResolve_NilParams(t *testing.T) {
	got, err := Resolve(context.Background(), nil, "", nil)
	require.NoError(t, err)
	assert.Nil(t, got, "nil input → nil output, no allocation")
}

func TestResolve_EmptyParams(t *testing.T) {
	in := map[string]string{}
	got, err := Resolve(context.Background(), in, "", nil)
	require.NoError(t, err)
	// Empty map is allowed to round-trip; the caller will see an
	// empty map either way.
	assert.Equal(t, 0, len(got))
}
