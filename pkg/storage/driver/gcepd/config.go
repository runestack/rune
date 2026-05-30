// Package gcepd implements the "gce-pd" storage driver, backed by Google
// Compute Engine Persistent Disks.
//
// The driver is registered under the name "gce-pd" (operator-facing,
// hyphenated) while the Go package itself is "gcepd" (Go forbids
// hyphens). Operators consume it by writing a StorageClass that points
// at it:
//
//	storageClass:
//	  name: pd-balanced-euw2a
//	  driver: gce-pd
//	  parameters:
//	    project: my-gcp-project       # optional on GCE (metadata fallback)
//	    zone: europe-west2-a
//	    diskType: pd-balanced
//	    fsType: ext4
//	  reclaimPolicy: retain
//	  allowedTopologies:
//	    - matchLabels:
//	        rune.io/zone: europe-west2-a
//
// Like Amazon EBS (and unlike DigitalOcean's region-scoped volumes), a
// zonal Persistent Disk is pinned to a single zone and can only attach to
// an instance in that same zone — hence `zone` is required at Provision
// time (sourced from parameters or the selected topology).
//
// Auth uses Application Default Credentials: on a GCE node whose service
// account carries the compute scopes (see terraform-google-rune) no
// credentials need to be configured at all — the metadata server serves
// them. For off-instance / cross-project controllers, a service-account
// key can be supplied via StorageClass `parameters.credentialsJSON`; the
// controller resolves any `secret:...` reference into plaintext before
// the driver sees it (RUNE-200 / pkg/storage/driverparams), so rotation
// flows through the secrets store with no driver-level wiring.
//
// Mirrors the do-volume / aws-ebs reference drivers (RUNE-069).
package gcepd

import (
	"fmt"
	"strings"
)

// DriverName is the registry key.
const DriverName = "gce-pd"

// Config is the runefile [storage.drivers.gce-pd] section after parsing.
// Only knobs that don't belong on per-StorageClass parameters live here —
// project, zone, disk type and auth are all sourced from StorageClass
// parameters so a single driver instance serves every project/zone.
type Config struct {
	// DiskNamePrefix is prepended to the name of every Persistent Disk
	// created by this driver, so multiple Rune clusters can share one GCP
	// project without their disk listings colliding. Default "rune-".
	DiskNamePrefix string
}

// parseConfig converts a raw map[string]any (viper output, or a test
// literal) into a *Config. Keys are matched case-insensitively to mirror
// the sibling driver parsers.
func parseConfig(raw map[string]any) (*Config, error) {
	cfg := &Config{DiskNamePrefix: "rune-"}
	if raw == nil {
		return cfg, nil
	}
	lc := make(map[string]any, len(raw))
	for k, v := range raw {
		lc[strings.ToLower(k)] = v
	}
	if v, ok := lc[strings.ToLower("diskNamePrefix")]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("gcepd: diskNamePrefix must be a string, got %T", v)
		}
		cfg.DiskNamePrefix = s
	}
	return cfg, nil
}
