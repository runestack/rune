package types

// LocalInstances is the per-node identity table maintained by the
// orchestrator and consumed by each agent. Each agent watches only its
// own node's record; the table maps a container IP observed at the
// agent's data plane to the (service, namespace) tuple that owns the
// container, so policy enforcement can resolve same-node source IPs
// back to a service identity without a service-mesh-style sidecar.
//
// Cross-node peer identity is intentionally not derived from this
// table: a connection arriving at an agent from a different node
// originates from the remote agent's proxy, not the original
// container. Cross-node policy uses CIDR/namespace selectors only
// until data-plane mTLS lands.
type LocalInstances struct {
	// NodeID is the identity of the node these instances live on.
	NodeID string `json:"nodeId" yaml:"nodeId"`

	// Instances maps a container IP (e.g. "172.17.0.5") to the
	// service identity that owns it.
	Instances map[string]InstanceIdentity `json:"instances" yaml:"instances"`
}

// InstanceIdentity is the minimum tuple needed to attribute a
// connection's source IP to a managed service.
type InstanceIdentity struct {
	// InstanceID is the orchestrator-assigned instance ID. Useful
	// for log correlation; not used for matching.
	InstanceID string `json:"instanceId" yaml:"instanceId"`

	// Service is the service name (no namespace).
	Service string `json:"service" yaml:"service"`

	// Namespace the service belongs to.
	Namespace string `json:"namespace" yaml:"namespace"`
}
