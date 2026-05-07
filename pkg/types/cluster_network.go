// Package types contains the canonical data model used across the Rune
// control plane and CLI. ClusterNetwork lives here so both the
// allocator (server side) and the CLI (rune admin network status) can
// share a single struct without an import cycle.
package types

// ClusterNetwork is the cluster-wide VIP allocation state. It is
// committed via the OrderedLog by the VIP allocator and surfaced
// through the Admin API for tooling. There is exactly one
// ClusterNetwork per cluster, keyed by the literal name "cluster".
//
// The allocator is the only writer; readers (CLI, dashboards) MUST
// treat the struct as opaque and avoid mutating it.
type ClusterNetwork struct {
	// CIDR is the service VIP range, e.g. "10.96.0.0/16". Set once at
	// bootstrap; changing it requires a cluster reset.
	CIDR string `json:"cidr"`

	// AllocatedVIPs maps serviceID → IP (string form, e.g. "10.96.0.7").
	// Stored as strings to keep JSON / proto round-trips stable.
	AllocatedVIPs map[string]string `json:"allocatedVIPs,omitempty"`

	// FreeList is the queue of IPs available for the next allocation.
	// IPs returned by Release reach FreeList only after the cooldown
	// period elapses. Stored as strings for the same reason as above.
	FreeList []string `json:"freeList,omitempty"`
}

// ClusterNetworkName is the canonical name of the singleton
// ClusterNetwork resource.
const ClusterNetworkName = "cluster"
