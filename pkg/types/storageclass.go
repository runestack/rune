// Package types — StorageClass resource definitions.
//
// Introduced in RUNE-073.
package types

import "time"

// StorageClass is a cluster-scoped resource describing how to provision
// persistent volumes via a particular storage Driver. Operators ship one
// StorageClass per cloud-provider+tier combination (e.g. "do-block-ssd",
// "aws-gp3-us-east-1"). The binary registers two built-in classes at boot:
// "local" (Default: true) and "local-host".
type StorageClass struct {
	// Unique identifier for the storage class.
	ID string `json:"id" yaml:"id"`

	// DNS-1123 cluster-unique name.
	Name string `json:"name" yaml:"name"`

	// Driver is the registered driver name (e.g. "local", "local-host",
	// "do-volume", "aws-ebs"). Resolved against the storage driver registry
	// at controller boot — unknown drivers fail validation.
	Driver string `json:"driver" yaml:"driver"`

	// Parameters are driver-specific key/value settings (filesystem type,
	// IOPS, region, etc.). Volume.Parameters override these per-volume.
	Parameters map[string]string `json:"parameters,omitempty" yaml:"parameters,omitempty"`

	// ReclaimPolicy is the default reclaim policy inherited by child
	// volumes. Volume.ReclaimPolicy overrides it.
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty" yaml:"reclaimPolicy,omitempty"`

	// AllowedTopologies constrains both provisioning and instance placement.
	// An empty slice means no topology restriction.
	AllowedTopologies []TopologySelector `json:"allowedTopologies,omitempty" yaml:"allowedTopologies,omitempty"`

	// Default indicates this is the cluster-default class. The API server
	// enforces an at-most-one invariant — setting Default: true on a second
	// class atomically clears the previous default. claimTemplate without an
	// explicit storageClassName resolves to whichever class currently has
	// Default: true.
	Default bool `json:"default,omitempty" yaml:"default,omitempty"`

	// Labels attached to the storage class.
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`

	// Creation timestamp.
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`

	// Last update timestamp.
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

// String returns the storage-class name (cluster-scoped, no namespace).
func (s *StorageClass) String() string { return s.Name }

// GetResourceType reports the resource type.
func (s *StorageClass) GetResourceType() ResourceType { return ResourceTypeStorageClass }

// Equals reports whether two StorageClass resources are functionally
// equivalent for watch purposes.
func (s *StorageClass) Equals(other Resource) bool {
	o, ok := other.(*StorageClass)
	if !ok {
		return false
	}
	if s.Name != o.Name || s.Driver != o.Driver || s.Default != o.Default ||
		s.ReclaimPolicy != o.ReclaimPolicy {
		return false
	}
	if !equalStringMap(s.Parameters, o.Parameters) {
		return false
	}
	if len(s.AllowedTopologies) != len(o.AllowedTopologies) {
		return false
	}
	return true
}

// TopologySelector matches nodes (and provisioning topologies) by label.
// MatchLabels and MatchExpressions are AND-ed together.
type TopologySelector struct {
	// MatchLabels is a map of {key: value} pairs. All pairs must match.
	MatchLabels map[string]string `json:"matchLabels,omitempty" yaml:"matchLabels,omitempty"`

	// MatchExpressions allows expressing set-based requirements (e.g.
	// "key rune.io/zone In [us-east-1a, us-east-1b]"). Use this when a
	// single class needs to span multiple zones — duplicate keys in
	// MatchLabels would silently collapse to the last value.
	MatchExpressions []TopologyMatchExpression `json:"matchExpressions,omitempty" yaml:"matchExpressions,omitempty"`
}

// TopologyMatchExpression is a set-based label requirement.
type TopologyMatchExpression struct {
	// Key is the label name (e.g. "rune.io/zone").
	Key string `json:"key" yaml:"key"`

	// Operator is one of: In, NotIn, Exists, DoesNotExist.
	Operator TopologyOperator `json:"operator" yaml:"operator"`

	// Values is the set of allowed/disallowed values. Required for In/NotIn,
	// must be empty for Exists/DoesNotExist.
	Values []string `json:"values,omitempty" yaml:"values,omitempty"`
}

// Matches reports whether the given label set satisfies the selector.
// MatchLabels and MatchExpressions are AND-ed: every key in MatchLabels
// must be present with the listed value, and every expression must
// evaluate true. An empty selector (no MatchLabels and no
// MatchExpressions) matches any label set.
//
// Operator semantics:
//   - In:           label is present and its value is one of Values.
//   - NotIn:        label is absent OR its value is not one of Values.
//   - Exists:       label key is present (Values ignored).
//   - DoesNotExist: label key is absent (Values ignored).
//
// An expression with an unknown Operator is treated as non-matching;
// callers that need stricter validation should validate selectors at
// admission time.
func (s TopologySelector) Matches(labels map[string]string) bool {
	for k, v := range s.MatchLabels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	for _, expr := range s.MatchExpressions {
		val, present := labels[expr.Key]
		switch expr.Operator {
		case TopologyOperatorIn:
			if !present {
				return false
			}
			if !containsString(expr.Values, val) {
				return false
			}
		case TopologyOperatorNotIn:
			if present && containsString(expr.Values, val) {
				return false
			}
		case TopologyOperatorExists:
			if !present {
				return false
			}
		case TopologyOperatorDoesNotExist:
			if present {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// containsString returns true if needle appears in haystack.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TopologyOperator names the set-based operators supported by
// TopologyMatchExpression.
type TopologyOperator string

const (
	TopologyOperatorIn           TopologyOperator = "In"
	TopologyOperatorNotIn        TopologyOperator = "NotIn"
	TopologyOperatorExists       TopologyOperator = "Exists"
	TopologyOperatorDoesNotExist TopologyOperator = "DoesNotExist"
)

// Well-known topology label keys reserved by the storage subsystem. Drivers
// and StorageClass.AllowedTopologies should use these keys.
const (
	// TopologyLabelRegion identifies a coarse-grained placement region
	// ("nyc3", "us-east-1", "hetzner-fsn1"). Set by operators on each node.
	TopologyLabelRegion = "rune.io/region"

	// TopologyLabelZone identifies a fine-grained availability zone
	// ("us-east-1a"). Required for AWS EBS / GCP PD topology constraints.
	TopologyLabelZone = "rune.io/zone"

	// TopologyLabelHostPathRoot identifies the host path root prefix that a
	// node has provisioned for local-host volumes ("/mnt/rune"). Used as
	// implicit affinity for the local-host driver.
	TopologyLabelHostPathRoot = "rune.io/host-path-root"
)

// equalStringMap is a small helper used by Equals implementations.
func equalStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
