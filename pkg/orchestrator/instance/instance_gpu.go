package instance

import (
	"context"
	"fmt"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator/gpu"
	"github.com/runestack/rune/pkg/store/repos"
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
// carries it from the start and needs no later binding. That matters for
// the crash window: a reservation written with its instance ID is
// reclaimable by the ordinary release path, where an instance-free one
// would wait for the sweep.
//
// A service with no gpu request does nothing here at all — no store read,
// no write, no error path.
func (c *Controller) reserveGPU(ctx context.Context, service *types.Service, instance *types.Instance) error {
	if service.Resources.GPU == nil || c.gpu == nil {
		return nil
	}

	node, err := c.nodeRecord(ctx)
	if err != nil {
		// Not "this node has no GPUs" — the node record is written by the
		// agent, which starts after the control plane, so this is the
		// window where the answer is not known yet. Returning it as a
		// capacity refusal would blame the driver on every restart.
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

// releaseGPU drops an instance's reservations.
//
// Driven by the TERMINAL STATUS transition, never by record deletion. A
// Failed instance is deliberately kept as its own tombstone for up to an
// hour; releasing on deletion would let a crash-looping engine hold its
// VRAM for that hour and block the replacement that would have fixed it.
//
// Best-effort by design: the caller is in the middle of writing a
// tombstone or stopping a container, and failing that because a ledger
// write failed would trade a leaked reservation for a stuck instance.
// The reclaim sweep is what catches whatever this misses.
func (c *Controller) releaseGPU(ctx context.Context, instance *types.Instance) {
	if c.gpu == nil || len(instance.GPUAssignments) == 0 {
		return
	}
	if err := c.gpu.Release(ctx, c.NodeID(), instance.ID); err != nil {
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
	if c.nodes == nil {
		c.nodes = repos.NewNodeRepo(c.store)
	}
	return c.nodes.Get(ctx, c.NodeID())
}
