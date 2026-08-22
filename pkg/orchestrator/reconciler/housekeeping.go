// Package reconciler — periodic sweeps: orphan cleanup, garbage collection of
// deleted and failed instances.
package reconciler

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
)

// runHousekeeping performs the one remaining cross-service sweep: reaping
// RUNNER-orphaned instances — instances a runner reports running that are
// absent from any service's desired state (runtime drift, e.g. a container
// that outlived its record). Runs on the periodic resync tick.
//
// The STORE-orphan sweep (instances whose parent service is gone) + its 90s
// grace window + inline volume reclaim were retired in RFC #129 Phase 4b:
// foreground deletion (reconcileDeletion) removes the service record only
// AFTER its instances and owned volumes are gone, so a store-orphan instance
// (or a leaked claimTemplate volume) can no longer exist by construction.
func (r *Reconciler) runHousekeeping(ctx context.Context) {
	r.logger.Debug("Starting housekeeping sweep")

	var services []types.Service
	if err := r.store.ListAll(ctx, types.ResourceTypeService, &services); err != nil {
		r.logger.Error("Failed to list services for housekeeping", log.Err(err))
		return
	}

	// Collect all orphaned instances across all services (instances found
	// running in a runner but absent from the desired state).
	var allOrphanedInstances []*types.Instance
	for i := range services {
		instanceData, err := r.getServiceInstances(ctx, &services[i])
		if err != nil {
			r.logger.Error("Failed to get service instances for orphaned detection",
				log.Str("service", services[i].Name),
				log.Err(err))
			continue
		}
		allOrphanedInstances = append(allOrphanedInstances, instanceData.OrphanedInstances...)
	}

	// Handle orphaned instances (running but not in desired state)
	if err := r.cleanUpOrphanedInstances(ctx, allOrphanedInstances); err != nil {
		r.logger.Error("Failed to clean up orphaned instances", log.Err(err))
	}

	r.logger.Debug("Housekeeping sweep completed")
}

func (r *Reconciler) cleanUpOrphanedInstances(ctx context.Context, orphanedInstances []*types.Instance) error {
	for _, instance := range orphanedInstances {
		if instance != nil {
			r.logger.Info("Cleaning up orphaned instance",
				log.Str("instance", instance.ID),
				log.Str("service", instance.ServiceID))
			if err := r.instanceController.DeleteInstance(ctx, instance); err != nil {
				r.logger.Error("Failed to clean up orphaned instance",
					log.Str("instance", instance.ID),
					log.Err(err))
				return err
			}

		}
	}
	return nil
}

// runGarbageCollection removes instances that have been marked as deleted
// after a specified retention period has passed
func (r *Reconciler) runGarbageCollection(ctx context.Context) error {
	r.logger.Debug("Running garbage collection")

	// Get all instances
	var instances []types.Instance
	err := r.store.ListAll(ctx, types.ResourceTypeInstance, &instances)
	if err != nil {
		return fmt.Errorf("failed to list instances for garbage collection: %w", err)
	}

	// Failed-instance retention pass. Done before the deleted-instance pass
	// below because we want any tombstones whose TTL has expired to be
	// promoted to Deleted so the existing deleted-instance retention can
	// keep cleaning them out of the store.
	r.gcFailedInstances(ctx, instances)

	// Filter for deleted instances
	for _, instance := range instances {
		if instance.Status == types.InstanceStatusDeleted && instance.Metadata != nil {
			// Check if retention period has passed
			if instance.Metadata.DeletionTimestamp == nil {
				// No timestamp, keep it for now and log the issue
				r.logger.Warn("Found deleted instance without deletion timestamp",
					log.Str("instance", instance.ID),
					log.Str("namespace", instance.Namespace))
				continue
			}

			// Parse the timestamp
			deletedAt := *instance.Metadata.DeletionTimestamp

			// Check if retention period has passed
			if time.Since(deletedAt) > deletedInstanceRetentionTime {
				r.logger.Info("Garbage collecting deleted instance",
					log.Str("instance", instance.ID),
					log.Str("service", instance.ServiceName),
					log.Str("namespace", instance.Namespace),
					log.Str("deletedAt", deletedAt.Format(time.RFC3339)),
					log.Str("age", time.Since(deletedAt).String()))

				// Remove from store
				if err := r.store.Delete(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID); err != nil {
					r.logger.Error("Failed to garbage collect instance",
						log.Str("instance", instance.ID),
						log.Err(err))
				} else {
					r.logger.Info("Successfully garbage collected instance",
						log.Str("instance", instance.ID))
				}
			}
		}
	}

	return nil
}

// gcFailedInstances evicts tombstoned Failed instances per the retention
// policy: per-service cap and TTL, whichever fires first. Eviction
// transitions a Failed instance to Deleted (with a DeletionTimestamp) so the
// existing deleted-instance retention sweep above picks it up on a later
// tick. Removing the underlying container is handled by
// instanceController.DeleteInstance.
func (r *Reconciler) gcFailedInstances(ctx context.Context, instances []types.Instance) {
	now := time.Now()

	// Bucket Failed-and-still-preserved instances by service.
	bySvc := map[string][]*types.Instance{}
	for i := range instances {
		inst := &instances[i]
		if inst.Status != types.InstanceStatusFailed || inst.FailedAt == nil {
			continue
		}
		key := inst.Namespace + "/" + inst.ServiceName
		bySvc[key] = append(bySvc[key], inst)
	}

	for key, tombs := range bySvc {
		// Sort logs-bearing first, then newest-first within each
		// group. The cap walks from index 0 and evicts beyond
		// per-service-cap, so this preferentially KEEPS tombstones
		// that have a captured stdout/stderr snapshot — exactly the
		// ones operators want for postmortems. Live observation:
		// prod/gateway's one informative crash (f67e328f, 14KB) was
		// previously evicted by the same cap that swept 6 silent
		// tombstones; with this ordering it survives until the TTL
		// fires (which is still respected below as a hard ceiling).
		sort.Slice(tombs, func(i, j int) bool {
			ihas := len(tombs[i].LastLogs) > 0
			jhas := len(tombs[j].LastLogs) > 0
			if ihas != jhas {
				return ihas
			}
			return tombs[i].FailedAt.After(*tombs[j].FailedAt)
		})

		for i, t := range tombs {
			tooOld := failedInstanceTTL > 0 && now.Sub(*t.FailedAt) > failedInstanceTTL
			beyondCap := failedInstancePerServiceCap > 0 && i >= failedInstancePerServiceCap
			if !tooOld && !beyondCap {
				continue
			}

			reason := "ttl"
			if beyondCap && !tooOld {
				reason = "cap"
			}
			r.logger.Info("Evicting failed-instance tombstone",
				log.Str("service", key),
				log.Str("tombstone_instance", t.ID),
				log.Str("container_id", t.ContainerID),
				log.Str("age", now.Sub(*t.FailedAt).String()),
				log.Str("reason", reason))

			// DeleteInstance stops + removes the container (idempotent
			// against an already-stopped container) and marks the store
			// record Deleted with a DeletionTimestamp. The deleted-
			// instance retention sweep cleans the row a few minutes later.
			if err := r.instanceController.DeleteInstance(ctx, t); err != nil {
				r.logger.Warn("Failed to evict failed-instance tombstone",
					log.Str("tombstone_instance", t.ID),
					log.Err(err))
			}
		}
	}
}
