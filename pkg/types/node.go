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
