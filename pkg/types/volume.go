// Package types — Volume resource definitions for the storage subsystem.
//
// Introduced in RUNE-069. See _docs/designs/RUNE-069-Storage-Management.md.
package types

import (
	"fmt"
	"path"
	"strings"
	"time"
)

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

// validVolumeAccessModes lists the AccessMode constants accepted on
// VolumeClaimTemplate. Drivers further restrict this set via their
// Capabilities; this is just the syntactic check the cast linter applies.
var validVolumeAccessModes = map[AccessMode]bool{
	AccessModeRWO: true,
	AccessModeROX: true,
	AccessModeRWX: true,
}

// validVolumeReclaimPolicies lists the ReclaimPolicy constants accepted on
// VolumeClaimTemplate.
var validVolumeReclaimPolicies = map[ReclaimPolicy]bool{
	ReclaimPolicyRetain: true,
	ReclaimPolicyDelete: true,
}

// ValidateVolumeMounts checks per-mount invariants that can be evaluated
// statically from the spec alone (no store / driver lookups). Used by both
// Service.Validate and ServiceSpec.Validate so the same rules fire from
// the API server's CreateService path and from `rune cast` / `rune lint`.
//
// Rules enforced:
//   - mount.Name and mount.MountPath are required.
//   - MountPath is absolute and not "/".
//   - SubPath, when set, is relative and contains no ".." segments.
//   - Exactly one of Claim / ClaimTemplate is set.
//   - Claim.Name is non-empty.
//   - ClaimTemplate.Size is non-empty and parses with ParseMemory.
//   - ClaimTemplate.AccessMode is required and a known constant.
//   - ClaimTemplate.ReclaimPolicy, if set, is a known constant.
//   - Mount.Name and MountPath are unique within the slice.
func ValidateVolumeMounts(mounts []VolumeMount) error {
	seenNames := make(map[string]bool, len(mounts))
	seenPaths := make(map[string]bool, len(mounts))
	for i, m := range mounts {
		if m.Name == "" {
			return NewValidationError(fmt.Sprintf("volume mount at index %d: name is required", i))
		}
		if seenNames[m.Name] {
			return NewValidationError(fmt.Sprintf("volume mount %q: duplicate name", m.Name))
		}
		seenNames[m.Name] = true

		if m.MountPath == "" {
			return NewValidationError(fmt.Sprintf("volume mount %q: mountPath is required", m.Name))
		}
		if !path.IsAbs(m.MountPath) {
			return NewValidationError(fmt.Sprintf("volume mount %q: mountPath %q must be absolute", m.Name, m.MountPath))
		}
		if path.Clean(m.MountPath) == "/" {
			return NewValidationError(fmt.Sprintf("volume mount %q: mountPath cannot be \"/\"", m.Name))
		}
		if seenPaths[m.MountPath] {
			return NewValidationError(fmt.Sprintf("volume mount %q: duplicate mountPath %q", m.Name, m.MountPath))
		}
		seenPaths[m.MountPath] = true

		if m.SubPath != "" {
			if path.IsAbs(m.SubPath) {
				return NewValidationError(fmt.Sprintf("volume mount %q: subPath %q must be relative", m.Name, m.SubPath))
			}
			for _, seg := range strings.Split(m.SubPath, "/") {
				if seg == ".." {
					return NewValidationError(fmt.Sprintf("volume mount %q: subPath %q cannot contain \"..\"", m.Name, m.SubPath))
				}
			}
		}

		switch {
		case m.Claim == nil && m.ClaimTemplate == nil:
			return NewValidationError(fmt.Sprintf("volume mount %q: exactly one of claim or claimTemplate must be set", m.Name))
		case m.Claim != nil && m.ClaimTemplate != nil:
			return NewValidationError(fmt.Sprintf("volume mount %q: claim and claimTemplate are mutually exclusive", m.Name))
		case m.Claim != nil:
			if strings.TrimSpace(m.Claim.Name) == "" {
				return NewValidationError(fmt.Sprintf("volume mount %q: claim.name is required", m.Name))
			}
		case m.ClaimTemplate != nil:
			ct := m.ClaimTemplate
			if strings.TrimSpace(ct.Size) == "" {
				return NewValidationError(fmt.Sprintf("volume mount %q: claimTemplate.size is required", m.Name))
			}
			if _, err := ParseMemory(ct.Size); err != nil {
				return NewValidationError(fmt.Sprintf("volume mount %q: invalid claimTemplate.size %q: %v", m.Name, ct.Size, err))
			}
			if ct.AccessMode == "" {
				return NewValidationError(fmt.Sprintf("volume mount %q: claimTemplate.accessMode is required", m.Name))
			}
			if !validVolumeAccessModes[ct.AccessMode] {
				return NewValidationError(fmt.Sprintf("volume mount %q: invalid claimTemplate.accessMode %q (allowed: ReadWriteOnce, ReadOnlyMany, ReadWriteMany)", m.Name, ct.AccessMode))
			}
			if ct.ReclaimPolicy != "" && !validVolumeReclaimPolicies[ct.ReclaimPolicy] {
				return NewValidationError(fmt.Sprintf("volume mount %q: invalid claimTemplate.reclaimPolicy %q (allowed: retain, delete)", m.Name, ct.ReclaimPolicy))
			}
		}
	}
	return nil
}

// systemMountBlocklist is the set of host paths that may never be used as a
// container mount-point. Mounting these confuses the runtime and is a
// common foot-gun (writing to /proc breaks PID semantics, mounting over
// /dev hides device nodes, exposing docker.sock is a sandbox escape).
var systemMountBlocklist = map[string]bool{
	"/proc":                       true,
	"/sys":                        true,
	"/dev":                        true,
	"/var/run/docker.sock":        true,
	"/run/docker.sock":            true,
	"/var/run/containerd.sock":    true,
	"/run/containerd/containerd.sock": true,
}

// ValidateMountPathConflicts enforces cast-time invariants that span multiple
// mount kinds on a single Service spec. All checks are statically evaluable
// — no store or driver lookups required.
//
// Rules enforced:
//   - No two mounts (volume + secret + configmap) may share a mountPath.
//   - No mountPath may sit inside (or equal) any path in
//     systemMountBlocklist (/proc, /sys, /dev, container-runtime sockets).
//   - When a service is RWO-attached and Scale > 1, the controller cannot
//     bind the same volume to N replicas; flag the spec at cast-time.
//
// The owner argument is a free-form label ("service foo", "job bar")
// included in error messages.
func ValidateMountPathConflicts(owner string, scale int, vols []VolumeMount, secs []SecretMount, cfgs []ConfigmapMount) error {
	type pathSrc struct {
		kind string
		name string
	}
	seen := make(map[string]pathSrc)
	check := func(kind, name, mp string) error {
		if mp == "" {
			return nil
		}
		clean := path.Clean(mp)
		// system-path blocklist (exact OR prefix).
		for blocked := range systemMountBlocklist {
			if clean == blocked || strings.HasPrefix(clean, blocked+"/") {
				return NewValidationError(fmt.Sprintf("%s: %s mount %q: mountPath %q is on the system blocklist (%s)", owner, kind, name, mp, blocked))
			}
		}
		if prev, ok := seen[clean]; ok {
			return NewValidationError(fmt.Sprintf("%s: %s mount %q: mountPath %q already used by %s mount %q", owner, kind, name, mp, prev.kind, prev.name))
		}
		seen[clean] = pathSrc{kind: kind, name: name}
		return nil
	}
	for _, m := range vols {
		if err := check("volume", m.Name, m.MountPath); err != nil {
			return err
		}
	}
	for _, m := range secs {
		if err := check("secret", m.Name, m.MountPath); err != nil {
			return err
		}
	}
	for _, m := range cfgs {
		if err := check("configmap", m.Name, m.MountPath); err != nil {
			return err
		}
	}
	// RWO + scale>1 invariant: a `claim:` reference (existing volume) at
	// scale>1 cannot bind, because RWO is single-attach. claimTemplate is
	// fine — each replica gets its own volume.
	if scale > 1 {
		for _, m := range vols {
			if m.Claim != nil {
				return NewValidationError(fmt.Sprintf("%s: volume mount %q: claim references a single volume but scale=%d (use claimTemplate for per-replica volumes)", owner, m.Name, scale))
			}
		}
	}
	return nil
}

// processRuntimeAllowedStorageClasses lists the StorageClass names a
// process-runtime service is permitted to bind to via claimTemplate.
// Process services run as host processes (no container mount namespace),
// so anything that needs a real driver mount (NFS, S3 FUSE, block
// devices, etc.) is meaningless or unsafe; only the local host-path
// drivers make sense.
var processRuntimeAllowedStorageClasses = map[string]struct{}{
	"":           {}, // empty == cluster default; resolved server-side. Allowed.
	"local":      {},
	"local-host": {},
}

// ValidateProcessRuntimeVolumes flags claimTemplate mounts on process-runtime
// services that target a StorageClass other than the local host-path
// classes. This is a static cast-time check intended to catch obvious
// misuse early — it does NOT cover Claim references to pre-existing
// volumes (those require a store lookup and are validated at bind time).
//
// The owner argument is a free-form label included in error messages.
func ValidateProcessRuntimeVolumes(owner string, runtime RuntimeType, vols []VolumeMount) error {
	if runtime != RuntimeTypeProcess {
		return nil
	}
	for _, m := range vols {
		if m.ClaimTemplate == nil {
			continue
		}
		sc := m.ClaimTemplate.StorageClassName
		if _, ok := processRuntimeAllowedStorageClasses[sc]; ok {
			continue
		}
		return NewValidationError(fmt.Sprintf(
			"%s: volume mount %q: storageClassName %q is not supported for runtime=process (use one of: local, local-host, or omit for cluster default)",
			owner, m.Name, sc))
	}
	return nil
}

