// Package types — Snapshot resource (skeleton; full impl in RUNE-071).
//
// The shape lives here so the storage Driver interface (RUNE-069) can refer
// to *types.Snapshot in Snapshot/RestoreFromSnapshot. RUNE-071 fills in the
// controller, gRPC service, and CLI; the resource itself is stable.
package types

import "time"

// SnapshotPhase is the lifecycle phase of a Snapshot resource.
type SnapshotPhase string

const (
	SnapshotPhasePending  SnapshotPhase = "Pending"
	SnapshotPhaseCreating SnapshotPhase = "Creating"
	SnapshotPhaseReady    SnapshotPhase = "Ready"
	SnapshotPhaseDeleting SnapshotPhase = "Deleting"
	SnapshotPhaseFailed   SnapshotPhase = "Failed"
)

// Snapshot is a namespace-scoped point-in-time copy of a Volume. Stored
// under snapshots/<ns>/<name> in the local store.
type Snapshot struct {
	NamespacedResource `json:"-" yaml:"-"`

	ID         string            `json:"id" yaml:"id"`
	Name       string            `json:"name" yaml:"name"`
	Namespace  string            `json:"namespace" yaml:"namespace"`
	Labels     map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`

	// SourceVolume is the bare name of the Volume in the same namespace
	// (cross-namespace snapshots are out of scope).
	SourceVolume string `json:"sourceVolume" yaml:"sourceVolume"`

	// Driver records which driver took the snapshot — needed at restore
	// time to route the call.
	Driver string `json:"driver" yaml:"driver"`

	// Handle is the driver-populated opaque snapshot identifier.
	Handle string `json:"handle,omitempty" yaml:"handle,omitempty"`

	// SizeBytes is the apparent size of the captured data.
	SizeBytes int64 `json:"sizeBytes,omitempty" yaml:"sizeBytes,omitempty"`

	// Scheduled is true if the snapshot was created by a SnapshotSchedule
	// rather than an explicit `rune snapshot create` call. Retention reaping
	// only considers scheduled snapshots.
	Scheduled bool `json:"scheduled,omitempty" yaml:"scheduled,omitempty"`

	Phase   SnapshotPhase `json:"phase" yaml:"phase"`
	Reason  string        `json:"reason,omitempty" yaml:"reason,omitempty"`
	Message string        `json:"message,omitempty" yaml:"message,omitempty"`

	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

// GetID implements NamespacedResource.
func (s *Snapshot) GetID() string { return s.ID }

// GetResourceType reports the resource type.
func (s *Snapshot) GetResourceType() ResourceType { return ResourceTypeSnapshot }

// String returns the namespaced identifier "<namespace>/<name>".
func (s *Snapshot) String() string { return s.Namespace + "/" + s.Name }

// Equals reports whether two Snapshot resources are functionally equivalent
// for watch purposes.
func (s *Snapshot) Equals(other Resource) bool {
	o, ok := other.(*Snapshot)
	if !ok {
		return false
	}
	return s.Name == o.Name &&
		s.Namespace == o.Namespace &&
		s.Phase == o.Phase &&
		s.Handle == o.Handle &&
		s.SizeBytes == o.SizeBytes
}
