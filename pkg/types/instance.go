package types

import (
	"fmt"
	"time"
)

// Validate that Instance implements the Resource interface
var _ Resource = (*Instance)(nil)

// Instance represents a running copy of a service.
type Instance struct {
	NamespacedResource `json:"-" yaml:"-"`

	// Runner type for the instance
	Runner RunnerType `json:"runner" yaml:"runner"`

	// Unique identifier for the instance
	ID string `json:"id" yaml:"id"`

	// Namespace of the instance
	Namespace string `json:"namespace" yaml:"namespace"`

	// Human-readable name for the instance
	Name string `json:"name" yaml:"name"`

	// ID of the service this instance belongs to
	ServiceID string `json:"serviceId" yaml:"serviceId"`

	// Name of the service this instance belongs to
	ServiceName string `json:"serviceName" yaml:"serviceName"`

	// Ordinal is the per-replica slot index (0-based) assigned by the
	// reconciler at creation. It is the authoritative source of per-instance
	// identity for stable resources — notably per-replica volume claimTemplates,
	// which bind to "<mount>-<service>-<ordinal>". Keeping the ordinal as an
	// explicit field (rather than parsing it back out of Name) decouples that
	// binding from the instance-name format, so the name can move to a unique
	// per-lifetime form (RUNE issue #84) without breaking volume rebinding.
	Ordinal int `json:"ordinal" yaml:"ordinal"`

	// Labels are denormalized from the parent Service's user labels at creation
	// (and, once a placement scheduler exists, will also carry the assigned
	// node's topology labels — see CreateInstance). They form the user-defined
	// LogQL stream dimensions for this instance's logs and the substrate for
	// future node-affinity / topology-spread scheduling. Low-cardinality only —
	// high-cardinality data belongs in log content/fields, not stream labels.
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`

	// ID of the node running this instance
	NodeID string `json:"nodeId" yaml:"nodeId"`

	// GPUAssignments are the device UUIDs this instance holds, in
	// assignment order. UUIDs rather than indices: the driver renumbers
	// indices across reboots, and both runners take UUIDs directly.
	//
	// A DENORMALISED CACHE for the runner, not the source of truth. The
	// node's device ledger is authoritative — this copy exists so the
	// runner can scope the container without reading it. When the two
	// disagree, the ledger wins and the reclaim sweep repairs this.
	GPUAssignments []string `json:"gpuAssignments,omitempty" yaml:"gpuAssignments,omitempty"`

	// IP address assigned to this instance
	IP string `json:"ip" yaml:"ip"`

	// Status of the instance
	Status InstanceStatus `json:"status" yaml:"status"`

	// StatusReason is a short, machine-friendly slug for the current
	// Status, regardless of whether Status is a terminal failure state.
	// Mirrors Service.StatusReason / Volume.StatusReason. Set by the
	// reconciler on every status transition — including non-terminal
	// states such as Pending while a precondition (volume, secret,
	// image) is still unmet. Empty only when Status is Running.
	// Vocabulary is the slug set produced by classifyCreateError
	// (e.g. "VolumeNotReady", "StorageClassMissing", "SecretNotFound").
	// On a Failed/Stalled instance this converges with FailureReason.
	StatusReason string `json:"statusReason,omitempty" yaml:"statusReason,omitempty"`

	// Detailed status information
	StatusMessage string `json:"statusMessage,omitempty" yaml:"statusMessage,omitempty"`

	// Container ID or process ID
	ContainerID string `json:"containerId,omitempty" yaml:"containerId,omitempty"`

	// Process ID for process runner
	PID int `json:"pid,omitempty" yaml:"pid,omitempty"`

	// Creation timestamp
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`

	// Last update timestamp
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`

	// LastTransitionAt is when Status last changed value. Drives the
	// "Pending for 5m" age in `rune describe` and lets the reconciler
	// distinguish a genuinely-stuck resource from a freshly-updated
	// one. Distinct from UpdatedAt, which moves on any field write.
	// nil until the first reconciler-observed transition.
	LastTransitionAt *time.Time `json:"lastTransitionAt,omitempty" yaml:"lastTransitionAt,omitempty"`

	// Process-specific configuration for process runner
	Process *ProcessSpec `json:"process,omitempty" yaml:"process,omitempty"`

	// Execution configuration for commands and environment
	Exec *Exec `json:"exec,omitempty" yaml:"exec,omitempty"`

	// Resources requirements for the instance
	Resources *Resources `json:"resources,omitempty" yaml:"resources,omitempty"`

	// Environment variables for the instance
	Environment map[string]string `json:"environment,omitempty" yaml:"environment,omitempty"`

	// Metadata contains additional information about the instance
	// Use for storing system properties that aren't part of the core spec
	Metadata *InstanceMetadata `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// InitStates records the per-instance execution state for each
	// of the parent service's InitSteps (RUNE-121). One entry per
	// declared step, in declaration order. Empty for instances of
	// services with no init steps.
	InitStates []InitStepState `json:"initStates,omitempty" yaml:"initStates,omitempty"`

	// SecurityContext is inherited from the parent service's
	// SecurityContext and applied to the main container by the runner.
	// nil means runtime defaults.
	SecurityContext *SecurityContext `json:"securityContext,omitempty" yaml:"securityContext,omitempty"`

	// FailedAt is the timestamp the instance entered the Failed state.
	// Used by the reconciler's failed-instance retention GC to drive
	// per-service cap + TTL eviction. nil for instances that have never
	// failed.
	FailedAt *time.Time `json:"failedAt,omitempty" yaml:"failedAt,omitempty"`

	// FailureReason is a short, machine-friendly slug describing why the
	// instance failed (e.g. "HealthCheckFailure", "ImagePullFailed",
	// "OOMKilled"). Set at the moment of the Failed transition.
	FailureReason string `json:"failureReason,omitempty" yaml:"failureReason,omitempty"`

	// Usage is a live resource-usage sample attached by the API read path
	// from runners that implement the StatsProvider capability. TRANSIENT:
	// excluded from store serialization (json/yaml "-") so stale samples
	// are never persisted — it exists only on instances flowing through a
	// read RPC response.
	Usage *InstanceUsage `json:"-" yaml:"-"`

	// LastLogs is a snapshot of the last N bytes of the instance's stdout
	// and stderr, captured before the container is removed (either during
	// a restart-on-failure cycle or at retention-GC eviction). Lets
	// `rune logs --previous` show what the dead container said even after
	// the container itself is gone. Bounded by runed config
	// (failedInstanceRetention.snapshotLogBytes, default 200 KB).
	LastLogs []byte `json:"lastLogs,omitempty" yaml:"lastLogs,omitempty"`

	// LastLogsCapturedAt records when LastLogs was snapshotted so the
	// CLI can show "logs as of <time>" instead of pretending they're live.
	LastLogsCapturedAt *time.Time `json:"lastLogsCapturedAt,omitempty" yaml:"lastLogsCapturedAt,omitempty"`

	// LastLogsTruncated is true when LastLogs is a tail-only snapshot
	// (we hit the byte cap). The CLI shows a "[truncated]" marker so
	// operators know there was more output above what's preserved.
	LastLogsTruncated bool `json:"lastLogsTruncated,omitempty" yaml:"lastLogsTruncated,omitempty"`

	// CreateAttempts counts how many times the orchestrator has tried
	// to stand this instance record up (resolve mounts → run init →
	// runner.Create → runner.Start). Persisted on the record so a runed
	// restart does not reset progress against a stable precondition
	// failure (e.g. StorageClassMissing). Zeroed on first success.
	CreateAttempts int `json:"createAttempts,omitempty" yaml:"createAttempts,omitempty"`

	// ContainerEverCreatedAt is the wall-clock time `runner.Create`
	// first succeeded for this instance ID. The reconciler uses this
	// to distinguish "create has never succeeded" (precondition
	// failure — keep retrying the same record) from "container
	// vanished" (docker rm, host reboot — tombstone and recreate).
	// nil until the first successful runner.Create.
	ContainerEverCreatedAt *time.Time `json:"containerEverCreatedAt,omitempty" yaml:"containerEverCreatedAt,omitempty"`

	// NextCreateAttemptAt is the earliest wall-clock time the
	// reconciler may try CreateInstance again for this record.
	// Populated after every failed attempt as an exponential backoff
	// (30s → 1m → 2m → 4m → 5m cap). Reconciler ticks before this
	// time leave the record alone. nil means "ready now" or "not in
	// retry mode". Cleared on first success and on operator restart.
	NextCreateAttemptAt *time.Time `json:"nextCreateAttemptAt,omitempty" yaml:"nextCreateAttemptAt,omitempty"`
}

func (i *Instance) GetResourceType() ResourceType {
	return ResourceTypeInstance
}

// InstanceMetadata contains additional information about the instance
type InstanceMetadata struct {
	// Image is the image that the instance is running
	Image string `json:"image,omitempty" yaml:"image,omitempty"`

	// Command is the executable to run inside the container,
	// propagated from Service.Command. Maps to Docker's Entrypoint:
	// it replaces the image's baked-in ENTRYPOINT. Empty leaves the
	// image's ENTRYPOINT untouched.
	Command string `json:"command,omitempty" yaml:"command,omitempty"`

	// Args are positional arguments to Command, propagated from
	// Service.Args. Maps to Docker's Cmd: it replaces the image's
	// baked-in CMD. nil leaves the image's CMD untouched.
	Args []string `json:"args,omitempty" yaml:"args,omitempty"`

	// ImagePull controls when the runner pulls the container image
	// ("always", "missing", "never"). Propagated from the parent
	// Service spec; empty defaults to "always".
	ImagePull string `json:"imagePull,omitempty" yaml:"imagePull,omitempty"`

	// ImagePullAnonymous mirrors Service.ImagePullAnonymous: pull this
	// image with no registry credentials at all.
	ImagePullAnonymous bool `json:"imagePullAnonymous,omitempty" yaml:"imagePullAnonymous,omitempty"`

	// ServiceGeneration is the generation of the service that the instance belongs to
	ServiceGeneration int64 `json:"serviceGeneration,omitempty" yaml:"serviceGeneration,omitempty"`

	// DeletionTimestamp is the timestamp when the instance was marked for deletion
	DeletionTimestamp *time.Time `json:"deletionTimestamp,omitempty" yaml:"deletionTimestamp,omitempty"`

	// RestartCount is the number of times this instance has been restarted
	RestartCount int `json:"restartCount,omitempty" yaml:"restartCount,omitempty"`

	// SecretMounts contains the resolved secret mount information for this instance
	SecretMounts []ResolvedSecretMount `json:"secretMounts,omitempty" yaml:"secretMounts,omitempty"`

	// ConfigmapMounts contains the resolved config mount information for this instance
	ConfigmapMounts []ResolvedConfigmapMount `json:"configMounts,omitempty" yaml:"configMounts,omitempty"`

	// VolumeMounts contains the resolved volume mount information for
	// this instance: each entry maps a Service.VolumeMount to the
	// concrete host-side source path produced by the storage driver.
	// Populated by the orchestrator's instance controller from the bound
	// Volume's Handle.
	VolumeMounts []ResolvedVolumeMount `json:"volumeMounts,omitempty" yaml:"volumeMounts,omitempty"`

	// Ports declared by the service (propagated for runner use)
	Ports []ServicePort `json:"ports,omitempty" yaml:"ports,omitempty"`

	// Expose specification from the service (propagated for runner use)
	Expose *ServiceExpose `json:"expose,omitempty" yaml:"expose,omitempty"`

	// Resolved exposed endpoint on host (best-effort)
	ExposedHost     string `json:"exposedHost,omitempty" yaml:"exposedHost,omitempty"`
	ExposedHostPort int    `json:"exposedHostPort,omitempty" yaml:"exposedHostPort,omitempty"`

	// ContainerIP is the IP assigned to the container on its primary
	// Docker network. Recorded by the runner on Start; consumed by the
	// agent to map source IPs to service identity for policy
	// enforcement.
	ContainerIP string `json:"containerIp,omitempty" yaml:"containerIp,omitempty"`
}

// ResolvedSecretMount contains the resolved secret data for mounting
type ResolvedSecretMount struct {
	// Name of the mount (for identification)
	Name string `json:"name" yaml:"name"`

	// Path where the secret should be mounted
	MountPath string `json:"mountPath" yaml:"mountPath"`

	// Resolved secret data (key -> value)
	Data map[string]string `json:"data" yaml:"data"`

	// Optional: specific keys to project from the secret
	Items []KeyToPath `json:"items,omitempty" yaml:"items,omitempty"`
}

// ResolvedConfigmapMount contains the resolved config data for mounting
type ResolvedConfigmapMount struct {
	// Name of the mount (for identification)
	Name string `json:"name" yaml:"name"`

	// Path where the config should be mounted
	MountPath string `json:"mountPath" yaml:"mountPath"`

	// Resolved config data (key -> value)
	Data map[string]string `json:"data" yaml:"data"`

	// Optional: specific keys to project from the config
	Items []KeyToPath `json:"items,omitempty" yaml:"items,omitempty"`
}

// ResolvedVolumeMount is the runner-facing representation of a Service
// VolumeMount after the orchestrator has resolved the underlying Volume
// to a concrete host-side source path.
//
// Source is whatever the storage driver's Mount step produced — for the
// in-tree "local" driver that's the managed directory under
// localVolumeRoot; for "local-host" it's the operator-declared host path;
// for cloud block-device drivers it would be the agent's per-volume mount
// target under /var/lib/rune/mounts/<volume-id>/.
type ResolvedVolumeMount struct {
	// Name is the mount's service-local identifier (matches
	// Service.VolumeMount.Name).
	Name string `json:"name" yaml:"name"`

	// MountPath is the absolute container path the volume is mounted at.
	MountPath string `json:"mountPath" yaml:"mountPath"`

	// Source is the host-side path to bind into the container.
	Source string `json:"source" yaml:"source"`

	// VolumeName is the name of the bound Volume resource (in the same
	// namespace as the owning service unless the mount used an FQDN).
	VolumeName string `json:"volumeName" yaml:"volumeName"`

	// VolumeNamespace is the namespace of the bound Volume.
	VolumeNamespace string `json:"volumeNamespace,omitempty" yaml:"volumeNamespace,omitempty"`

	// ReadOnly mounts the volume read-only inside the container.
	ReadOnly bool `json:"readOnly,omitempty" yaml:"readOnly,omitempty"`

	// SubPath, if non-empty, selects a sub-path within the volume.
	SubPath string `json:"subPath,omitempty" yaml:"subPath,omitempty"`
}

// Exec represents execution configuration for a command
type Exec struct {
	// Command to execute
	Command []string `json:"command" yaml:"command"`

	// Environment variables
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}

// InstanceStatus represents the current status of an instance.
type InstanceStatus string

const (
	// InstanceStatusPending indicates the instance is being created.
	InstanceStatusPending InstanceStatus = "Pending"

	// InstanceStatusRunning indicates the instance is running.
	InstanceStatusRunning InstanceStatus = "Running"

	// InstanceStatusStopped indicates the instance has stopped.
	InstanceStatusStopped InstanceStatus = "Stopped"

	// InstanceStatusFailed indicates the instance failed to start or crashed.
	InstanceStatusFailed InstanceStatus = "Failed"

	// InstanceStatusDeleted indicates the instance has been marked for deletion
	// but is retained in the store for a period before garbage collection.
	InstanceStatusDeleted InstanceStatus = "Deleted"

	// InstanceStatusTerminating indicates the instance is actively being
	// torn down — runner.Stop/Remove are in flight but haven't completed.
	// Without this, an instance whose parent service has just been
	// deleted (or which the reconciler is otherwise GC'ing) keeps
	// reporting Running until the graceful-shutdown timeout finishes,
	// giving operators a misleading "Running" view for ~10s.
	// Mirrors K8s' Pod.Phase=Terminating.
	InstanceStatusTerminating InstanceStatus = "Terminating"

	// InstanceStatusStalled indicates create has failed too many times
	// with a stable precondition error (StorageClassMissing, secret
	// missing, image-pull error) and the reconciler has stopped
	// auto-retrying until an operator intervenes. The slot is still
	// held by this record; operators unstick it with
	// `rune restart instance` or `rune cast` (new service generation).
	// Mirrors the volume controller's ProvisionRetriesExhausted shape.
	InstanceStatusStalled InstanceStatus = "Stalled"

	// Process runner specific statuses
	InstanceStatusCreated  InstanceStatus = "Created"
	InstanceStatusStarting InstanceStatus = "Starting"
	InstanceStatusExited   InstanceStatus = "Exited"
	InstanceStatusUnknown  InstanceStatus = "Unknown"
)

// InstanceStatusInfo contains information about an instance's status
type InstanceStatusInfo struct {
	Status        InstanceStatus
	StatusMessage string
	InstanceID    string
	NodeID        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Validate validates the instance configuration.
func (i *Instance) Validate() error {
	if i.ID == "" {
		return NewValidationError("instance ID is required")
	}

	if i.Namespace == "" {
		return NewValidationError("instance namespace is required")
	}

	if i.Name == "" {
		return NewValidationError("instance name is required")
	}

	if i.ServiceID == "" {
		return NewValidationError("instance serviceId is required")
	}

	if i.NodeID == "" {
		return NewValidationError("instance nodeId is required")
	}

	return nil
}

// String returns a unique identifier for the instance
func (i *Instance) String() string {
	return fmt.Sprintf("%s/%s", i.Namespace, i.ID)
}

// Equals checks if two instances are functionally equivalent for watch purposes
func (i *Instance) Equals(other Resource) bool {
	otherInstance, ok := other.(*Instance)
	if !ok {
		return false
	}

	// Check key fields that would make an instance visibly different in the table
	return i.ID == otherInstance.ID &&
		i.Name == otherInstance.Name &&
		i.Namespace == otherInstance.Namespace &&
		i.ServiceID == otherInstance.ServiceID &&
		i.NodeID == otherInstance.NodeID &&
		i.Status == otherInstance.Status
}

// InstanceUsage is a point-in-time resource-usage sample for one instance,
// reported by a runner that implements the StatsProvider capability.
type InstanceUsage struct {
	// CPUPercent is CPU usage as a share of the WHOLE HOST, 0–100 — the
	// same denominator as node-level CPU on HealthService, so instance
	// bars and node bars compare 1:1. Negative when unknown.
	CPUPercent float64

	// MemUsedBytes is memory used in bytes (cgroup usage minus inactive
	// file cache, matching `docker stats` semantics).
	MemUsedBytes uint64

	// MemLimitBytes is the container's cgroup memory limit, which equals
	// the host total when the container is uncapped. 0 when unknown.
	MemLimitBytes uint64
}
