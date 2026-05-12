// Package driver defines the storage driver interface that the Rune
// VolumeController and node-side agent talk to. Built-in drivers live in
// subpackages (driver/local, driver/dovolume, ...) and register themselves
// with the registry from init().
//
// Introduced in RUNE-069. See _docs/designs/RUNE-069-Storage-Management.md.
package driver

import (
	"context"
	"errors"
	"time"

	"github.com/runestack/rune/pkg/types"
)

// Driver is the contract every storage backend implements. The interface is
// deliberately not CSI: it captures only what Rune needs in v1 and keeps the
// surface narrow enough for in-process Go drivers. A future CSI shim driver
// can wrap external CSI plugins behind this same interface.
//
// The leader-side VolumeController calls Provision, Delete, Snapshot,
// RestoreFromSnapshot and Expand. Node-side agents call Attach, Detach,
// Mount and Unmount. Capabilities and Name are pure metadata.
//
// All methods MUST be context-aware and idempotent: the controller and agent
// retry on transient failures.
type Driver interface {
	// Name returns the registered driver name (e.g. "local", "do-volume").
	// Must match the registry key.
	Name() string

	// Capabilities describes what this driver supports. Returned values are
	// expected to be stable for the lifetime of the process.
	Capabilities() Capabilities

	// Provision creates a new backing store for the volume and returns an
	// opaque handle that subsequent calls (Attach, Snapshot, ...) re-parse.
	// MUST be idempotent on Volume.ID — the controller retries on failure.
	Provision(ctx context.Context, req ProvisionRequest) (VolumeHandle, error)

	// Delete destroys the backing store identified by handle. Implementations
	// MUST tolerate missing/already-deleted handles (return nil).
	Delete(ctx context.Context, handle VolumeHandle) error

	// Attach makes the volume available to the named node (e.g. attaches a
	// cloud block device, opens an iSCSI session, no-ops for local
	// directories). Returns the device path the node should Mount.
	// For drivers that don't have a separate attach step, return an empty
	// DevicePath and a nil error.
	Attach(ctx context.Context, handle VolumeHandle, node NodeID) (DevicePath, error)

	// Detach is the inverse of Attach. Idempotent.
	Detach(ctx context.Context, handle VolumeHandle, node NodeID) error

	// Mount makes the volume usable at MountTarget. For block-device drivers
	// this typically formats (if needed) and mounts; for directory-style
	// drivers it bind-mounts or returns the source path the runner should
	// bind-mount itself. The returned MountTarget is what the runner uses —
	// drivers may rewrite it (e.g. local-host returns the host path
	// directly rather than the proposed Target).
	Mount(ctx context.Context, opts MountOpts) (MountTarget, error)

	// Unmount is the inverse of Mount. Idempotent.
	Unmount(ctx context.Context, target MountTarget) error

	// Snapshot creates a point-in-time snapshot of the volume. Drivers
	// without snapshot support (Capabilities.Snapshots == false) MUST return
	// ErrUnsupported.
	Snapshot(ctx context.Context, req SnapshotRequest) (SnapshotHandle, error)

	// RestoreFromSnapshot provisions a NEW volume from a snapshot. The
	// returned handle replaces the volume's existing handle.
	// Drivers without snapshot support MUST return ErrUnsupported.
	RestoreFromSnapshot(ctx context.Context, req RestoreRequest) (VolumeHandle, error)

	// DeleteSnapshot destroys the snapshot identified by handle. MUST be
	// idempotent — implementations should swallow ErrNotFound and return
	// nil so the controller can re-drive a Deleting snapshot safely.
	// Drivers without snapshot support MUST return ErrUnsupported.
	DeleteSnapshot(ctx context.Context, handle SnapshotHandle) error

	// Expand grows the volume to NewSize. Online expansion vs offline is
	// driver-dependent; drivers that only support offline expand (e.g.
	// do-volume) MUST refuse if the volume is currently Bound and return
	// ErrOnlineExpandUnsupported.
	// Drivers without expand support (Capabilities.Expand == false) MUST
	// return ErrUnsupported.
	Expand(ctx context.Context, handle VolumeHandle, newSize string) error
}

// Capabilities describes the optional features a driver supports.
type Capabilities struct {
	// AccessModes is the set of access modes the driver can satisfy.
	AccessModes []types.AccessMode

	// Snapshots indicates Snapshot/RestoreFromSnapshot are implemented.
	Snapshots bool

	// Expand indicates Expand is implemented (some drivers only support
	// offline expand — see Driver.Expand docs).
	Expand bool

	// OnlineExpand is true if Expand may be called while the volume is
	// Bound (without first detaching).
	OnlineExpand bool

	// BlockDevice is true if the driver exposes a raw block device the
	// runner is expected to format/mount itself. Directory-style drivers
	// (local, local-host, NFS) set this to false.
	BlockDevice bool

	// TopologyKeys are the well-known label keys this driver consumes from
	// StorageClass.AllowedTopologies. Used by the API-server linter to
	// reject unknown keys early.
	TopologyKeys []string
}

// VolumeHandle is the opaque, driver-owned identifier returned by Provision
// and stored on Volume.Handle. The controller treats it as a black-box
// string; the owning driver re-parses it.
type VolumeHandle string

// SnapshotHandle is the opaque driver identifier for a snapshot.
type SnapshotHandle string

// NodeID identifies a node in the Rune cluster (matches types.Node.ID).
type NodeID string

// DevicePath is a node-local block device path returned by Attach (e.g.
// "/dev/disk/by-id/scsi-0DO_Volume_flo-data"). Empty for directory drivers.
type DevicePath string

// MountTarget is a node-local filesystem path the runner consumes when it
// bind-mounts the volume into a container.
type MountTarget string

// ProvisionRequest is the input to Driver.Provision. The controller fills
// every field from the Volume + StorageClass before calling.
type ProvisionRequest struct {
	// Volume is the resource being provisioned. Drivers MUST treat it as
	// read-only; mutations belong on the returned VolumeHandle / on a
	// controller-driven Update call.
	Volume *types.Volume

	// StorageClass is the class the Volume references (already resolved by
	// the controller).
	StorageClass *types.StorageClass

	// MergedParameters is StorageClass.Parameters with Volume.Parameters
	// overlaid on top — drivers should consult this rather than re-merging.
	MergedParameters map[string]string

	// SizeBytes is Volume.Size already parsed into bytes by the controller.
	// Drivers that need the original string can read Volume.Size.
	SizeBytes int64

	// Topology is the topology constraint the controller selected for this
	// provision (the chosen TopologySelector after intersecting
	// StorageClass.AllowedTopologies with node availability). Empty means
	// no constraint.
	Topology *types.TopologySelector

	// Deadline is a soft deadline for the operation. Drivers should respect
	// ctx.Done() but may use this for finer-grained internal timeouts.
	Deadline time.Time
}

// MountOpts is the input to Driver.Mount.
type MountOpts struct {
	Volume   *types.Volume
	Handle   VolumeHandle
	Node     NodeID
	Device   DevicePath  // device returned by Attach (empty for directory drivers)
	Target   MountTarget // proposed mount target (may be rewritten by driver)
	ReadOnly bool
	// FsType is the filesystem to format with on first mount of a block
	// device. Ignored by directory drivers. Sourced from StorageClass /
	// Volume parameters.
	FsType string
}

// SnapshotRequest is the input to Driver.Snapshot.
type SnapshotRequest struct {
	Volume   *types.Volume
	Handle   VolumeHandle
	Snapshot *types.Snapshot
}

// RestoreRequest is the input to Driver.RestoreFromSnapshot.
type RestoreRequest struct {
	// Source is the snapshot being restored.
	Source       *types.Snapshot
	SourceHandle SnapshotHandle
	// Target is the new Volume that should receive the restored data.
	Target           *types.Volume
	StorageClass     *types.StorageClass
	MergedParameters map[string]string
	SizeBytes        int64
}

// Sentinel errors. Drivers SHOULD wrap these with %w when returning richer
// context so callers can use errors.Is.
var (
	// ErrUnsupported is returned by drivers that lack the requested
	// capability (e.g. local-host calling Snapshot).
	ErrUnsupported = errors.New("storage driver: operation unsupported")

	// ErrNotFound is returned when a handle has no backing store. Delete
	// implementations SHOULD swallow this internally and return nil to
	// preserve idempotency.
	ErrNotFound = errors.New("storage driver: handle not found")

	// ErrInvalidConfig is returned for misconfigured StorageClass /
	// Volume parameters (bad fsType, missing required field, ...).
	ErrInvalidConfig = errors.New("storage driver: invalid configuration")

	// ErrOnlineExpandUnsupported is returned by Expand when the driver
	// requires the volume to be detached first.
	ErrOnlineExpandUnsupported = errors.New("storage driver: online expand unsupported")

	// ErrAccessModeUnsupported is returned by Provision when the requested
	// AccessMode is not in Capabilities.AccessModes.
	ErrAccessModeUnsupported = errors.New("storage driver: access mode unsupported")
)
