package instance

import (
	"context"
	"fmt"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/gpu"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// reserveGPU claims device capacity for an instance about to be created,
// and records the devices on it.
//
// Called BEFORE the instance record is written. The reservation is the
// admission check — if it refuses, no instance record should exist,
// because a record that cannot be placed is a tombstone the operator has
// to clean up for a decision Rune already made.
//
// The instance's UUID is minted before any of this, so the reservation
// carries its owner from the moment it is written and needs no later
// binding step. A crash between this and the store write still strands
// the row — nothing reclaims those yet.
func (c *Controller) reserveGPU(ctx context.Context, service *types.Service, instance *types.Instance) error {
	if service.Resources.GPU == nil || c.gpu == nil {
		return nil
	}

	node, err := c.nodeRecord(ctx)
	if err != nil {
		// No record yet is the same state as a record with no probe stamp,
		// and has to carry the same retryable reason: the agent writes it
		// and starts after the control plane, so every restart passes
		// through this window. A capacity refusal here would blame the
		// driver for a race with startup.
		if store.IsNotFoundError(err) {
			return gpu.ErrInventoryUnknown()
		}
		return fmt.Errorf("gpu: %w", err)
	}

	placement, err := c.gpu.Reserve(ctx, node, gpu.Request{
		NodeID:      c.NodeID(),
		Namespace:   instance.Namespace,
		ServiceName: instance.ServiceName,
		InstanceID:  instance.ID,
		GPU:         *service.Resources.GPU,
	})
	if err != nil {
		return err
	}

	instance.GPUAssignments = placement.DeviceUUIDs
	if placement.CrossNamespace {
		// A shared device is a shared trust domain, and the operator
		// should learn it from a log line rather than from an incident.
		c.logger.Info("GPU placed on a device held by another namespace",
			log.Str("instance", instance.Name),
			log.Str("namespace", instance.Namespace),
			log.Any("devices", placement.DeviceUUIDs),
			log.Any("other_namespaces", placement.OtherNamespaces))
	}
	c.logger.Info("Reserved GPU capacity",
		log.Str("instance", instance.Name),
		log.Any("devices", placement.DeviceUUIDs))
	return nil
}

// releaseGPU drops an instance's reservations. Called at every status
// transition an instance does not come back from, including Deleted —
// see Admitter.Release for why release follows the status rather than the
// removal of the record.
//
// Best-effort: the caller is mid-way through a tombstone write or a
// container teardown, and failing that over a ledger write would trade a
// leaked reservation for a stuck instance. Nothing sweeps up what this
// misses, so the Warn below is the only signal an operator gets.
func (c *Controller) releaseGPU(ctx context.Context, instance *types.Instance) {
	if c.gpu == nil || len(instance.GPUAssignments) == 0 {
		return
	}
	// The instance's own node, not this controller's: the two are the same
	// today, and this is what keeps being true when they stop being.
	nodeID := instance.NodeID
	if nodeID == "" {
		nodeID = c.NodeID()
	}
	if err := c.gpu.Release(ctx, nodeID, instance.ID); err != nil {
		c.logger.Warn("Failed to release GPU reservation",
			log.Str("instance", instance.ID),
			log.Any("devices", instance.GPUAssignments),
			log.Err(err))
		return
	}
	c.logger.Info("Released GPU capacity",
		log.Str("instance", instance.Name),
		log.Any("devices", instance.GPUAssignments))
}

// nodeRecord reads this node's inventory.
func (c *Controller) nodeRecord(ctx context.Context) (*types.Node, error) {
	return c.nodes.Get(ctx, c.NodeID())
}
