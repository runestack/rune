// Package hcloudvolume implements the "hcloud-volume" storage driver —
// Rune's Hetzner Cloud Block Storage driver. It mirrors the shape of
// dovolume: per-call auth sourced from StorageClass `parameters.apiToken`,
// region pinning (Hetzner calls it `location`), and offline-only expand.
//
// The driver is registered under the name "hcloud-volume" (operator-facing,
// hyphenated) while the Go package itself is "hcloudvolume" (Go forbids
// hyphens). Operators consume it by writing a StorageClass that points
// at it:
//
//	storageClass:
//	  name: hcloud-ssd-nbg1
//	  driver: hcloud-volume
//	  parameters:
//	    location: nbg1
//	    fsType: ext4
//	    apiToken: "secret:hcloud-api-token.shared.rune/token"
//	  reclaimPolicy: retain
//	  allowedTopologies:
//	    - matchLabels:
//	        rune.io/region: nbg1
//
// Auth uses a Hetzner Cloud API Token sourced from the StorageClass
// `parameters.apiToken` value. The controller resolves any `secret:...`
// reference into plaintext before the driver sees it.
//
// Hetzner Cloud volumes do NOT support snapshots — the driver advertises
// Capabilities.Snapshots = false and Snapshot / RestoreFromSnapshot /
// DeleteSnapshot all return driver.ErrUnsupported. Expand is supported
// but offline-only: the volume must be detached for the resize action
// to succeed.
package hcloudvolume

import (
	"fmt"
	"strings"
)

// DriverName is the registry key.
const DriverName = "hcloud-volume"

// Config is the runefile [storage.drivers.hcloud-volume] section after
// parsing. Only knobs that don't belong on per-StorageClass parameters
// live here — auth is sourced from StorageClass `parameters.apiToken`.
type Config struct {
	// APIBaseURL overrides the Hetzner Cloud API endpoint. Defaults to
	// "https://api.hetzner.cloud" — only set in tests against a
	// httptest.Server.
	APIBaseURL string

	// VolumeNamePrefix is prepended to every Hetzner volume name created
	// by this driver, so multiple Rune clusters can share one Hetzner
	// project without colliding. Default "rune-".
	VolumeNamePrefix string
}

// parseConfig converts a raw map[string]any (viper output, or a test
// literal) into a *Config. Keys are matched case-insensitively.
func parseConfig(raw map[string]any) (*Config, error) {
	cfg := &Config{
		APIBaseURL:       "https://api.hetzner.cloud",
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
			return nil, fmt.Errorf("hcloudvolume: apiBaseURL must be a string, got %T", v)
		}
		if s != "" {
			cfg.APIBaseURL = strings.TrimRight(s, "/")
		}
	}
	if v, ok := get("volumeNamePrefix"); ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("hcloudvolume: volumeNamePrefix must be a string, got %T", v)
		}
		cfg.VolumeNamePrefix = s
	}
	return cfg, nil
}
