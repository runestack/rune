package types

import (
	"time"
)

// Node represents a machine that can run service instances.
type Node struct {
	// Unique identifier for the node
	ID string `json:"id" yaml:"id"`

	// Human-readable name for the node
	Name string `json:"name" yaml:"name"`

	// IP address or hostname of the node
	Address string `json:"address" yaml:"address"`

	// Labels attached to the node for scheduling decisions
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`

	// Available resources on this node
	Resources NodeResources `json:"resources" yaml:"resources"`

	// Status of the node
	Status NodeStatus `json:"status" yaml:"status"`

	// StatusReason is a short, machine-friendly slug explaining the
	// current Status (e.g. "HeartbeatTimeout", "DrainRequested").
	// Mirrors Service/Instance/Volume StatusReason. Empty when the
	// node is Ready.
	StatusReason string `json:"statusReason,omitempty" yaml:"statusReason,omitempty"`

	// StatusMessage is a human-readable sentence explaining StatusReason,
	// surfaced by `rune describe node`.
	StatusMessage string `json:"statusMessage,omitempty" yaml:"statusMessage,omitempty"`

	// Creation timestamp
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`

	// Last heartbeat timestamp
	LastHeartbeat time.Time `json:"lastHeartbeat" yaml:"lastHeartbeat"`

	// Devices is the GPU inventory the node's own agent probed
	// (RUNE-301 §6.1). Empty on every machine without a GPU, which is
	// the normal result and not an error — see DeviceProbeError.
	//
	// Devices hangs off Node rather than NodeResources because
	// NodeResources is capacity scalars and this is a list of
	// identities; the proto name NodeResources is also already taken by
	// an unrelated live-health message.
	Devices []GPUDevice `json:"devices,omitempty" yaml:"devices,omitempty"`

	// DevicesProbedAt is when the device probe last returned. NIL MEANS
	// NEVER PROBED, which is a different state from "probed and found
	// nothing" and must never be read as "this node has no GPUs"
	// (RUNE-301 §5.3, D27) — the agent starts after the control plane,
	// so every restart has a window where the answer is not known yet.
	DevicesProbedAt *time.Time `json:"devicesProbedAt,omitempty" yaml:"devicesProbedAt,omitempty"`

	// DeviceProbeError is why the last probe failed, verbatim; empty
	// means it succeeded. Without it "no devices" collapses six distinct
	// causes — no driver, nvidia-smi absent from PATH, permission denied
	// to the rune user, a probe hung on an Xid, CSV drift on a new
	// driver, and not-probed-yet — into one unactionable string
	// (RUNE-301 §5.3, §11.2).
	DeviceProbeError string `json:"deviceProbeError,omitempty" yaml:"deviceProbeError,omitempty"`
}

// GPUDevice is one physical accelerator as the node's probe reported it
// (RUNE-301 §6.1). Vendor-neutral by construction; the only v1 probe is
// NVIDIA.
type GPUDevice struct {
	// UUID is the device identity — stable across reboots and
	// renumbering. Everything that refers to a device refers to this.
	UUID string `json:"uuid" yaml:"uuid"`

	// Index is the driver's ordinal. DISPLAY ONLY: it is not stable and
	// is never an identity.
	Index int `json:"index" yaml:"index"`

	// Vendor is the lowercase vendor slug, e.g. "nvidia".
	Vendor string `json:"vendor" yaml:"vendor"`

	// Product is the marketing name as the driver reports it, e.g.
	// "NVIDIA L40S".
	Product string `json:"product" yaml:"product"`

	// VRAMBytes is total device memory, as the driver reports it.
	VRAMBytes int64 `json:"vramBytes" yaml:"vramBytes"`

	// DriverVersion is the kernel driver version, e.g. "550.54.15".
	DriverVersion string `json:"driverVersion" yaml:"driverVersion"`

	// CUDAVersion is the CUDA runtime version the driver supports, when
	// the probe could determine it.
	CUDAVersion string `json:"cudaVersion,omitempty" yaml:"cudaVersion,omitempty"`

	// Missing marks a device seen by an earlier probe and absent now
	// (RUNE-301 §11.4). Reserved for P3; P1 never sets it.
	Missing bool `json:"missing,omitempty" yaml:"missing,omitempty"`
}

// NodeStatus represents the current status of a node.
type NodeStatus string

const (
	// NodeStatusReady indicates the node is ready to accept instances.
	NodeStatusReady NodeStatus = "Ready"

	// NodeStatusNotReady indicates the node is not ready.
	NodeStatusNotReady NodeStatus = "NotReady"

	// NodeStatusDraining indicates the node is being drained of instances.
	NodeStatusDraining NodeStatus = "Draining"
)

// NodeResources represents the resources available on a node.
type NodeResources struct {
	// Available CPU in millicores (1000m = 1 CPU)
	CPU int64 `json:"cpu" yaml:"cpu"`

	// Available memory in bytes
	Memory int64 `json:"memory" yaml:"memory"`
}

// Validate validates the node configuration.
func (n *Node) Validate() error {
	if n.ID == "" {
		return NewValidationError("node ID is required")
	}

	if n.Name == "" {
		return NewValidationError("node name is required")
	}

	if n.Address == "" {
		return NewValidationError("node address is required")
	}

	return nil
}
