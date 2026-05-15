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
// Every operation method takes an OpContext as the second argument (after
// ctx). The controller / agent populates it with the resolved StorageClass,
// the Volume row, and the merged parameter map. Drivers consult OpContext
// for any per-class / per-volume configuration they need (region, fsType,
// auth references, …) — the v0.0.1-dev.46 fix routed Volume.Size into
// the driver layer the same way; this generalises the pattern to all
// driver methods so per-class config no longer has to live in the
// runefile only. See RUNE-200.
//
// All methods MUST be context-aware and idempotent: the controller and
// agent retry on transient failures.
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
	Provision(ctx context.Context, opctx OpContext, req ProvisionRequest) (VolumeHandle, error)

	// Delete destroys the backing store identified by handle. Implementations
	// MUST tolerate missing/already-deleted handles (return nil).
	Delete(ctx context.Context, opctx OpContext, handle VolumeHandle) error

	// Attach makes the volume available to the named node (e.g. attaches a
	// cloud block device, opens an iSCSI session, no-ops for local
	// directories). Returns the device path the node should Mount.
	// For drivers that don't have a separate attach step, return an empty
	// DevicePath and a nil error.
	Attach(ctx context.Context, opctx OpContext, handle VolumeHandle, node NodeID) (DevicePath, error)

	// Detach is the inverse of Attach. Idempotent.
	Detach(ctx context.Context, opctx OpContext, handle VolumeHandle, node NodeID) error

	// Mount makes the volume usable at MountTarget. For block-device drivers
	// this typically formats (if needed) and mounts; for directory-style
	// drivers it bind-mounts or returns the source path the runner should
	// bind-mount itself. The returned MountTarget is what the runner uses —
	// drivers may rewrite it (e.g. local-host returns the host path
	// directly rather than the proposed Target).
	Mount(ctx context.Context, opctx OpContext, opts MountOpts) (MountTarget, error)

	// Unmount is the inverse of Mount. Idempotent.
	Unmount(ctx context.Context, opctx OpContext, target MountTarget) error

	// Snapshot creates a point-in-time snapshot of the volume. Drivers
	// without snapshot support (Capabilities.Snapshots == false) MUST return
	// ErrUnsupported.
	Snapshot(ctx context.Context, opctx OpContext, req SnapshotRequest) (SnapshotHandle, error)

	// RestoreFromSnapshot provisions a NEW volume from a snapshot. The
	// returned handle replaces the volume's existing handle. OpContext
	// here describes the TARGET volume + its class — the source snapshot
	// is carried on RestoreRequest.
	// Drivers without snapshot support MUST return ErrUnsupported.
	RestoreFromSnapshot(ctx context.Context, opctx OpContext, req RestoreRequest) (VolumeHandle, error)

	// DeleteSnapshot destroys the snapshot identified by handle. MUST be
	// idempotent — implementations should swallow ErrNotFound and return
	// nil so the controller can re-drive a Deleting snapshot safely.
	// Drivers without snapshot support MUST return ErrUnsupported.
	DeleteSnapshot(ctx context.Context, opctx OpContext, handle SnapshotHandle) error

	// Expand grows the volume to NewSize. Online expansion vs offline is
	// driver-dependent; drivers that only support offline expand (e.g.
	// do-volume) MUST refuse if the volume is currently Bound and return
	// ErrOnlineExpandUnsupported.
	// Drivers without expand support (Capabilities.Expand == false) MUST
	// return ErrUnsupported.
	Expand(ctx context.Context, opctx OpContext, handle VolumeHandle, newSize string) error
}

// OpContext is the per-call context every Driver method receives as the
// second positional argument (after ctx). The controller / agent builds
// it before each call so drivers can consult per-class / per-volume
// configuration without holding it as instance state. Drivers MUST treat
// OpContext fields as read-only.
//
// StorageClass MAY be nil — orphan deletes (the class was deleted before
// its volumes were reclaimed) carry only Volume + Parameters, where
// Parameters comes from Volume.Metadata.DriverParameters as a snapshot
// taken at Provision time (see RUNE-200 PR 2).
//
// Parameters is always populated (may be empty). Drivers MUST consult
// Parameters rather than re-merging StorageClass.Parameters with
// Volume.Parameters themselves.
type OpContext struct {
	// StorageClass that this operation is acting on. May be nil for
	// orphan deletes (class removed before its volumes). Live operations
	// always carry the resolved class.
	StorageClass *types.StorageClass

	// Volume is the row this operation pertains to. Always non-nil for
	// volume-scoped operations (every method on the interface).
	Volume *types.Volume

	// Parameters is StorageClass.Parameters with Volume.Parameters
	// overlaid on top, pre-merged by the caller. Drivers consult this
	// map for per-class configuration (region, fsType, auth refs, etc.)
	// rather than re-merging themselves.
	Parameters map[string]string

	// NodeHostname is the OS hostname (os.Hostname()) of the agent
	// running this call, populated by the controller / agent at
	// build time. Cloud-backed drivers use it to map Rune's node
	// identity onto the cloud provider's instance identity — DO
	// droplets, for example, are addressable by their hostname-derived
	// name via /v2/droplets?name=... Empty when the caller has no
	// hostname to report (controller-only operations like Provision /
	// Delete that don't run on a specific node, or tests).
	NodeHostname string
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

// ProvisionRequest is the input to Driver.Provision. Volume, StorageClass
// and merged parameters live on the accompanying OpContext; this struct
// carries only fields specific to the provision operation itself.
type ProvisionRequest struct {
	// SizeBytes is Volume.Size already parsed into bytes by the controller.
	// Drivers that need the original string can read OpContext.Volume.Size.
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

// MountOpts is the input to Driver.Mount. The Volume lives on the
// accompanying OpContext.
type MountOpts struct {
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

// SnapshotRequest is the input to Driver.Snapshot. The Volume being
// snapshotted lives on the accompanying OpContext.
type SnapshotRequest struct {
	Handle   VolumeHandle
	Snapshot *types.Snapshot
}

// RestoreRequest is the input to Driver.RestoreFromSnapshot. The TARGET
// volume and its storage class live on the accompanying OpContext; this
// struct carries only the source snapshot reference.
type RestoreRequest struct {
	// Source is the snapshot being restored.
	Source       *types.Snapshot
	SourceHandle SnapshotHandle

	// SizeBytes is the target Volume.Size already parsed into bytes.
	SizeBytes int64
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
