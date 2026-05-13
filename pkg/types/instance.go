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

	// ID of the node running this instance
	NodeID string `json:"nodeId" yaml:"nodeId"`

	// IP address assigned to this instance
	IP string `json:"ip" yaml:"ip"`

	// Status of the instance
	Status InstanceStatus `json:"status" yaml:"status"`

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
}

func (i *Instance) GetResourceType() ResourceType {
	return ResourceTypeInstance
}

// InstanceMetadata contains additional information about the instance
type InstanceMetadata struct {
	// Image is the image that the instance is running
	Image string `json:"image,omitempty" yaml:"image,omitempty"`

	// ImagePull controls when the runner pulls the container image
	// ("always", "missing", "never"). Propagated from the parent
	// Service spec; empty defaults to "always".
	ImagePull string `json:"imagePull,omitempty" yaml:"imagePull,omitempty"`

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
