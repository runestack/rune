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
//	    apiToken: "secret:do-api-token.shared.rune/token"
//	  reclaimPolicy: retain
//	  allowedTopologies:
//	    - matchLabels:
//	        rune.io/region: nyc3
//
// Auth uses a DigitalOcean Personal Access Token sourced from the
// StorageClass `parameters.apiToken` value. The controller resolves any
// `secret:...` reference into plaintext before the driver sees it
// (RUNE-200 PR 3 / pkg/storage/driverparams), so token rotation flows
// through the secrets store with no driver-level wiring.
//
// Introduced in RUNE-069. See _docs/designs/RUNE-069-Storage-Management.md §5.1.
package dovolume

import (
	"fmt"
	"strings"
)

// DriverName is the registry key.
const DriverName = "do-volume"

// Config is the runefile [storage.drivers.do-volume] section after
// parsing. Only knobs that don't belong on per-StorageClass parameters
// live here — auth is sourced from StorageClass `parameters.apiToken`
// (resolved by the controller-side secret resolver).
type Config struct {
	// APIBaseURL overrides the DO API endpoint. Defaults to
	// "https://api.digitalocean.com" — only set in tests against a
	// httptest.Server.
	APIBaseURL string

	// VolumeNamePrefix is prepended to every DO volume name created by
	// this driver, so multiple Rune clusters can share one DO
	// account without colliding. Default "rune-".
	VolumeNamePrefix string
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
	return cfg, nil
}
