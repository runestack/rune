// Package wiring holds the orchestrator's inbound seams — the
// interfaces the runed agent implements and wires into a live
// orchestrator after start (RUNE-311 Phase 4). They live here, not in
// the instance package, so internal/agent/* implements them without
// importing instance-lifecycle API, and not in the orchestrator package
// because the instance controller consumes them (which would cycle).
package wiring

import (
	"context"

	"github.com/runestack/rune/pkg/types"
)

// MountResolver is the orchestrator-side surface of the agent's
// volumes Subsystem. The runed agent supplies an implementation
// backed by internal/agent/volumes.Subsystem.
type MountResolver interface {
	// MountTargetFor returns the host-side mount target for the named
	// volume on this node, plus a presence flag. A false return means
	// the subsystem has not (yet) mounted the volume locally; callers
	// should treat this as a transient "not ready" condition.
	MountTargetFor(volumeID string) (string, bool)
}

// MountErrorReporter is an optional companion to MountResolver that explains
// WHY a volume is not mounted. Without it, an instance blocked on storage only
// ever reported "not yet mounted (will retry)", which reads as a benign
// transient. In practice an expired cloud credential once stranded volumes
// that were attached and mounted the whole time, with the real HTTP 401
// visible only in the agent's startup log — undiagnosable from the CLI.
// Implementations return the most recent bring-up failure for the volume.
type MountErrorReporter interface {
	MountErrorFor(volumeID string) (string, bool)
}

// EndpointPublisher is the orchestrator-side surface of the
// networking data plane. The runed agent supplies an implementation
// backed by pkg/networking/endpoints + pkg/networking/localinstances.
type EndpointPublisher interface {
	// PublishService re-publishes the full Endpoint set for a service.
	// `endpoints` may be empty (the service has no running instances)
	// in which case the publisher should clear/Delete.
	PublishService(ctx context.Context, service *types.Service, endpoints []types.Endpoint) error
	// PublishLocalInstances re-publishes the per-node identity table
	// for `nodeID` so that policy enforcement can map source IPs back
	// to a service identity.
	PublishLocalInstances(ctx context.Context, nodeID string, table map[string]types.InstanceIdentity) error
}
