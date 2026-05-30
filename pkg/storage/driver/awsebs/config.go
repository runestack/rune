// Package awsebs implements the "aws-ebs" storage driver, backed by
// Amazon EBS (Elastic Block Store) volumes.
//
// The driver is registered under the name "aws-ebs" (operator-facing,
// hyphenated) while the Go package itself is "awsebs" (Go forbids
// hyphens). Operators consume it by writing a StorageClass that points
// at it:
//
//	storageClass:
//	  name: ebs-gp3-euw2a
//	  driver: aws-ebs
//	  parameters:
//	    region: eu-west-2
//	    availabilityZone: eu-west-2a
//	    volumeType: gp3
//	    fsType: ext4
//	  reclaimPolicy: retain
//	  allowedTopologies:
//	    - matchLabels:
//	        rune.io/zone: eu-west-2a
//
// Unlike DigitalOcean Block Storage (which is region-scoped), an EBS
// volume is pinned to a single Availability Zone and can only attach to
// an instance in that same AZ — hence `availabilityZone` is required at
// Provision time (sourced from parameters or the selected topology).
//
// Auth uses the standard AWS credential chain: on an EC2 node with an
// instance profile (see terraform-aws-rune's IAM role +
// AmazonEC2ContainerRegistryReadOnly / EBS permissions) no credentials
// need to be configured at all. For cross-account or off-instance
// controllers, static credentials can be supplied via StorageClass
// `parameters.accessKeyId` / `secretAccessKey` / `sessionToken`; the
// controller resolves any `secret:...` reference into plaintext before
// the driver sees it (RUNE-200 / pkg/storage/driverparams), so rotation
// flows through the secrets store with no driver-level wiring.
//
// Mirrors the do-volume reference driver (RUNE-069).
package awsebs

import (
	"fmt"
	"strings"
)

// DriverName is the registry key.
const DriverName = "aws-ebs"

// Config is the runefile [storage.drivers.aws-ebs] section after
// parsing. Only knobs that don't belong on per-StorageClass parameters
// live here — region, AZ, volume type and auth are all sourced from
// StorageClass parameters (resolved by the controller-side secret
// resolver) so a single driver instance serves every region/account.
type Config struct {
	// VolumeNamePrefix is prepended to the Name tag of every EBS volume
	// created by this driver, so multiple Rune clusters can share one
	// AWS account without their console listings colliding. EBS volumes
	// have no unique-name constraint (identity is the volume ID), so this
	// is cosmetic plus a cheap cluster filter. Default "rune-".
	VolumeNamePrefix string
}

// parseConfig converts a raw map[string]any (viper output, or a test
// literal) into a *Config. Keys are matched case-insensitively to mirror
// the do-volume / local driver parsers — viper lowercases nested map
// keys, while driver tests typically use the camelCase spelling
// published in the design doc.
func parseConfig(raw map[string]any) (*Config, error) {
	cfg := &Config{
		VolumeNamePrefix: "rune-",
	}
	if raw == nil {
		return cfg, nil
	}
	lc := make(map[string]any, len(raw))
	for k, v := range raw {
		lc[strings.ToLower(k)] = v
	}
	if v, ok := lc[strings.ToLower("volumeNamePrefix")]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("awsebs: volumeNamePrefix must be a string, got %T", v)
		}
		cfg.VolumeNamePrefix = s
	}
	return cfg, nil
}
