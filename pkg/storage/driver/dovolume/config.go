// Package dovolume implements the "do-volume" storage driver — Rune's
// reference cloud driver, backed by DigitalOcean Block Storage.
//
// The driver is registered under the name "do-volume" (operator-facing,
// hyphenated) while the Go package itself is "dovolume" (Go forbids
// hyphens). Operators consume it by writing a StorageClass that points
// at it:
//
//	storageClass:
//	  name: do-ssd-nyc3
//	  driver: do-volume
//	  parameters:
//	    region: nyc3
//	    fsType: ext4
//	  reclaimPolicy: retain
//	  allowedTopologies:
//	    - matchLabels:
//	        rune.io/region: nyc3
//
// Auth uses a DigitalOcean Personal Access Token. The token may be
// supplied two ways via the runefile [storage.drivers.do-volume]
// section:
//
//   - `apiToken: "dop_v1_..."` — literal token (typically operator
//     environment substitution, e.g. ${env:DO_API_TOKEN}). Use for
//     dev / single-tenant clusters.
//   - `apiTokenSecretRef: "<ns>/<name>#<key>"` — reference to a Rune
//     Secret. Resolved at every API call so token rotation works
//     without restarting runed. Requires a SecretLookup to be
//     injected into the factory config under the reserved key
//     "_secretLookup" (cmd/runed does this from a SecretRepo).
//
// Introduced in RUNE-069. See _docs/designs/RUNE-069-Storage-Management.md §5.1.
package dovolume

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// DriverName is the registry key.
const DriverName = "do-volume"

// ConfigKeySecretLookup is the reserved key the factory map[string]any
// carries the SecretLookup callable under. Wired by cmd/runed; tests
// supply their own. Using a leading underscore signals "not a runefile
// field" — viper would never produce a key beginning with one.
const ConfigKeySecretLookup = "_secretLookup"

// configKeySecretLookup is kept as a private alias for in-package use.
const configKeySecretLookup = ConfigKeySecretLookup

// SecretLookup resolves a single field from a Rune Secret. namespace +
// name identify the Secret; key selects a field of Secret.Data. Returns
// the plaintext value or an error.
//
// Drivers call this synchronously on every Provision / Attach / etc. so
// secret rotation takes effect on the next reconcile without requiring
// a runed restart.
type SecretLookup func(ctx context.Context, namespace, name, key string) (string, error)

// Config is the runefile [storage.drivers.do-volume] section after
// parsing. Every field is optional at parse time; required-fields
// validation happens at first use so a misconfigured do-volume stanza
// doesn't crash runed for clusters that don't use DO.
type Config struct {
	// APIToken is a literal DO Personal Access Token. Mutually
	// exclusive with APITokenSecretRef.
	APIToken string

	// APITokenSecretRef references a Rune Secret. Format:
	// "<namespace>/<name>#<key>". Resolved via SecretLookup at call
	// time. Mutually exclusive with APIToken.
	APITokenSecretRef string

	// APIBaseURL overrides the DO API endpoint. Defaults to
	// "https://api.digitalocean.com" — only set in tests against a
	// httptest.Server.
	APIBaseURL string

	// VolumeNamePrefix is prepended to every DO volume name created by
	// this driver, so multiple Rune clusters can share one DO
	// account without colliding. Default "rune-".
	VolumeNamePrefix string

	// SecretLookup is injected by cmd/runed (or tests). nil is
	// allowed at parse time; resolveToken() returns a clear error if
	// APITokenSecretRef is set without it.
	SecretLookup SecretLookup
}

// parseConfig converts a raw map[string]any (viper output, or a test
// literal) into a *Config. Keys are matched case-insensitively to mirror
// the local driver's parser — viper lowercases nested map keys, while
// driver tests typically use the camelCase spelling published in the
// design doc.
func parseConfig(raw map[string]any) (*Config, error) {
	cfg := &Config{
		APIBaseURL:       "https://api.digitalocean.com",
		VolumeNamePrefix: "rune-",
	}
	if raw == nil {
		return cfg, nil
	}
	lc := make(map[string]any, len(raw))
	for k, v := range raw {
		lc[strings.ToLower(k)] = v
	}
	get := func(camel string) (any, bool) {
		v, ok := lc[strings.ToLower(camel)]
		return v, ok
	}
	if v, ok := get("apiToken"); ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("dovolume: apiToken must be a string, got %T", v)
		}
		cfg.APIToken = strings.TrimSpace(s)
	}
	if v, ok := get("apiTokenSecretRef"); ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("dovolume: apiTokenSecretRef must be a string, got %T", v)
		}
		cfg.APITokenSecretRef = strings.TrimSpace(s)
	}
	if v, ok := get("apiBaseURL"); ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("dovolume: apiBaseURL must be a string, got %T", v)
		}
		if s != "" {
			cfg.APIBaseURL = strings.TrimRight(s, "/")
		}
	}
	if v, ok := get("volumeNamePrefix"); ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("dovolume: volumeNamePrefix must be a string, got %T", v)
		}
		cfg.VolumeNamePrefix = s
	}
	// Reserved key — never operator-supplied via the runefile, only
	// injected programmatically by cmd/runed (or tests).
	if v, ok := raw[configKeySecretLookup]; ok && v != nil {
		fn, ok := v.(SecretLookup)
		if !ok {
			return nil, fmt.Errorf("dovolume: %s must be a dovolume.SecretLookup, got %T", configKeySecretLookup, v)
		}
		cfg.SecretLookup = fn
	}
	if cfg.APIToken != "" && cfg.APITokenSecretRef != "" {
		return nil, fmt.Errorf("dovolume: apiToken and apiTokenSecretRef are mutually exclusive")
	}
	// If neither apiToken nor apiTokenSecretRef is set we defer the
	// error to first use — if no DO StorageClass is declared, we should
	// not block runed startup. resolveToken surfaces the missing-config
	// error on the first request.
	return cfg, nil
}

// resolveToken returns the API token to use for a single DO API call.
// Called on every request so rotated secrets take effect on the next
// reconcile. Returns a sentinel-wrapped error when the configuration
// is incomplete — the caller surfaces it through the driver's error
// path (Provision → controller → Volume.Reason).
func (c *Config) resolveToken(ctx context.Context) (string, error) {
	if c.APIToken != "" {
		return c.APIToken, nil
	}
	if c.APITokenSecretRef == "" {
		return "", errors.New("dovolume: no apiToken or apiTokenSecretRef configured")
	}
	if c.SecretLookup == nil {
		return "", errors.New("dovolume: apiTokenSecretRef set but no SecretLookup wired (runed bug)")
	}
	ns, name, key, err := parseSecretRef(c.APITokenSecretRef)
	if err != nil {
		return "", err
	}
	tok, err := c.SecretLookup(ctx, ns, name, key)
	if err != nil {
		return "", fmt.Errorf("dovolume: resolve apiTokenSecretRef %q: %w", c.APITokenSecretRef, err)
	}
	if strings.TrimSpace(tok) == "" {
		return "", fmt.Errorf("dovolume: secret %q field %q is empty", ns+"/"+name, key)
	}
	return tok, nil
}

// parseSecretRef splits "<ns>/<name>#<key>" into its three parts.
// Whitespace inside any segment is rejected.
func parseSecretRef(ref string) (ns, name, key string, err error) {
	hashIdx := strings.IndexByte(ref, '#')
	if hashIdx < 0 {
		return "", "", "", fmt.Errorf("dovolume: apiTokenSecretRef %q missing '#<key>' suffix", ref)
	}
	left := ref[:hashIdx]
	key = ref[hashIdx+1:]
	slashIdx := strings.IndexByte(left, '/')
	if slashIdx < 0 {
		return "", "", "", fmt.Errorf("dovolume: apiTokenSecretRef %q missing '<namespace>/' prefix", ref)
	}
	ns = left[:slashIdx]
	name = left[slashIdx+1:]
	if ns == "" || name == "" || key == "" {
		return "", "", "", fmt.Errorf("dovolume: apiTokenSecretRef %q has empty namespace/name/key segment", ref)
	}
	if strings.ContainsAny(ref, " \t\n") {
		return "", "", "", fmt.Errorf("dovolume: apiTokenSecretRef %q contains whitespace", ref)
	}
	return ns, name, key, nil
}
