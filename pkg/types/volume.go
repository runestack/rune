// Package types — Volume resource definitions for the storage subsystem.
//
// Introduced in RUNE-069. See _docs/designs/RUNE-069-Storage-Management.md.
package types

import "time"

// AccessMode describes how a Volume may be mounted by attached instances.
type AccessMode string

const (
	// AccessModeRWO allows read/write by a single instance at a time.
	AccessModeRWO AccessMode = "ReadWriteOnce"
	// AccessModeROX allows read-only mount by many instances simultaneously.
	AccessModeROX AccessMode = "ReadOnlyMany"
	// AccessModeRWX allows read/write by many instances simultaneously.
	// Only honoured by drivers whose Capabilities advertise it (NFS, etc.).
	AccessModeRWX AccessMode = "ReadWriteMany"
)

// ReclaimPolicy controls what happens to the underlying storage when the
// owning Volume (or owning Service for claimTemplate-created volumes) is
// deleted. See RUNE-069 §7 for trigger semantics; reclaim never fires on
// instance death.
type ReclaimPolicy string

const (
	// ReclaimPolicyRetain leaves the underlying storage in place; the Volume
	// resource is removed but data is preserved for manual recovery.
	ReclaimPolicyRetain ReclaimPolicy = "retain"
	// ReclaimPolicyDelete asks the driver to destroy the underlying storage
	// when the Volume is reclaimed.
	ReclaimPolicyDelete ReclaimPolicy = "delete"
)

// VolumeStatus is the lifecycle phase of a Volume resource.
type VolumeStatus string

const (
	VolumeStatusPending      VolumeStatus = "Pending"
	VolumeStatusProvisioning VolumeStatus = "Provisioning"
	VolumeStatusAvailable    VolumeStatus = "Available"
	VolumeStatusBound        VolumeStatus = "Bound"
	VolumeStatusReleased     VolumeStatus = "Released"
	VolumeStatusStalled      VolumeStatus = "Stalled"
	VolumeStatusFailed       VolumeStatus = "Failed"
)

// Volume is a namespace-scoped persistent storage resource managed by a
// Driver. Stored under volumes/<ns>/<name> in the local store.
type Volume struct {
	NamespacedResource `json:"-" yaml:"-"`

	// Unique identifier for the volume.
	ID string `json:"id" yaml:"id"`

	// DNS-1123 unique name within the namespace.
	Name string `json:"name" yaml:"name"`

	// Namespace the volume belongs to.
	Namespace string `json:"namespace" yaml:"namespace"`

	// Labels attached to the volume for organization.
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`

	// StorageClassName references the StorageClass used to provision this
	// volume. The class's driver, parameters, reclaimPolicy and
	// allowedTopologies are inherited unless overridden on the Volume.
	StorageClassName string `json:"storageClassName" yaml:"storageClassName"`

	// Size as a human-readable string (e.g. "10Gi"). Validation lives in the
	// API server / cast linter.
	Size string `json:"size" yaml:"size"`

	// AccessMode requested by the workload. Must be in the driver's
	// Capabilities.AccessModes.
	AccessMode AccessMode `json:"accessMode" yaml:"accessMode"`

	// ReclaimPolicy overrides the inherited StorageClass policy for this
	// volume only. Empty defers to the StorageClass.
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty" yaml:"reclaimPolicy,omitempty"`

	// Parameters are driver-specific overrides merged on top of
	// StorageClass.Parameters. Example: hostPath for the local-host driver.
	Parameters map[string]string `json:"parameters,omitempty" yaml:"parameters,omitempty"`

	// SnapshotSchedule, when set, instructs the controller to create
	// scheduled snapshots via the worker pool.
	SnapshotSchedule *SnapshotSchedule `json:"snapshotSchedule,omitempty" yaml:"snapshotSchedule,omitempty"`

	// Handle is the driver-populated opaque identifier returned by Provision.
	// The controller treats it as a string; the owning driver re-parses it.
	Handle string `json:"handle,omitempty" yaml:"handle,omitempty"`

	// OwnerService is set on volumes auto-created from a claimTemplate so the
	// controller knows whose deletion may trigger reclaim.
	// Format: "<namespace>/<service-name>".
	OwnerService string `json:"ownerService,omitempty" yaml:"ownerService,omitempty"`

	// BoundNode is the node where the volume is currently attached, if any.
	BoundNode string `json:"boundNode,omitempty" yaml:"boundNode,omitempty"`

	// BoundClaim records what the volume is bound to. Format:
	// "<service>/<mountName>[/<ordinal>]".
	BoundClaim string `json:"boundClaim,omitempty" yaml:"boundClaim,omitempty"`

	// Status is the lifecycle phase. Controller-managed.
	Status VolumeStatus `json:"status" yaml:"status"`

	// Reason is a short machine-readable code explaining the status (e.g.
	// "HostPathMissing", "ProvisionTimeout").
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`

	// Message is a human-readable description of the status.
	Message string `json:"message,omitempty" yaml:"message,omitempty"`

	// Creation timestamp.
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`

	// Last update timestamp.
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

// GetID implements NamespacedResource.
func (v *Volume) GetID() string { return v.ID }

// GetResourceType implements NamespacedResource.
func (v *Volume) GetResourceType() ResourceType { return ResourceTypeVolume }

// String returns the namespaced identifier "<namespace>/<name>".
func (v *Volume) String() string { return v.Namespace + "/" + v.Name }

// Equals reports whether two Volume resources are functionally equivalent
// for watch purposes.
func (v *Volume) Equals(other Resource) bool {
	o, ok := other.(*Volume)
	if !ok {
		return false
	}
	return v.Name == o.Name &&
		v.Namespace == o.Namespace &&
		v.Status == o.Status &&
		v.BoundNode == o.BoundNode &&
		v.BoundClaim == o.BoundClaim &&
		v.Handle == o.Handle &&
		v.Size == o.Size
}

// VolumeMount declares a volume reference inside a Service or Job container.
// Exactly one of Claim or ClaimTemplate must be set; the other is nil.
type VolumeMount struct {
	// Name is a service-local identifier for the mount.
	Name string `json:"name" yaml:"name"`

	// MountPath is the absolute container path the volume is mounted at.
	MountPath string `json:"mountPath" yaml:"mountPath"`

	// ReadOnly mounts the volume read-only inside the container.
	ReadOnly bool `json:"readOnly,omitempty" yaml:"readOnly,omitempty"`

	// SubPath optionally selects a sub-path within the volume to mount
	// (parity with k8s subPath; useful for sharing one volume across mounts).
	SubPath string `json:"subPath,omitempty" yaml:"subPath,omitempty"`

	// Claim references an existing Volume by name (bare name resolves to the
	// service's namespace; "<ns>/<name>" supports cross-namespace claims).
	Claim *VolumeClaim `json:"claim,omitempty" yaml:"claim,omitempty"`

	// ClaimTemplate provisions per-replica volumes, k8s-StatefulSet style.
	ClaimTemplate *VolumeClaimTemplate `json:"claimTemplate,omitempty" yaml:"claimTemplate,omitempty"`
}

// VolumeClaim is a reference to an existing Volume.
type VolumeClaim struct {
	// Name is the bare name (resolved in the service's namespace) or
	// "<namespace>/<name>" for cross-namespace claims.
	Name string `json:"name" yaml:"name"`
}

// VolumeClaimTemplate causes the controller to auto-provision one Volume per
// replica of the owning service. The generated volume name is
// "<mount>-<service>-<ordinal>" with OwnerService set on each.
type VolumeClaimTemplate struct {
	StorageClassName string            `json:"storageClassName,omitempty" yaml:"storageClassName,omitempty"`
	Size             string            `json:"size" yaml:"size"`
	AccessMode       AccessMode        `json:"accessMode" yaml:"accessMode"`
	Parameters       map[string]string `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	ReclaimPolicy    ReclaimPolicy     `json:"reclaimPolicy,omitempty" yaml:"reclaimPolicy,omitempty"`
}

// SnapshotSchedule configures recurring driver snapshots for a Volume.
type SnapshotSchedule struct {
	// Cron expression in standard 5-field format ("0 2 * * *").
	Cron string `json:"cron" yaml:"cron"`

	// Retention is the number of historical snapshots to keep. Older
	// snapshots are reaped after each successful new snapshot.
	Retention int `json:"retention,omitempty" yaml:"retention,omitempty"`
}
