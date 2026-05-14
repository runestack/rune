package finalizers

import (
	"context"
	"fmt"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// VolumeCleanupFinalizer removes Volume rows whose OwnerService matches
// the service being deleted. The VolumeController's WatchEventDeleted
// handler then runs the per-volume reclaim policy: drivers honour
// ReclaimPolicyDelete (call driver.Delete on the handle) and skip
// ReclaimPolicyRetain (leave the handle in place for later salvage).
//
// Volumes without OwnerService set are NOT touched: those are operator-
// owned resources whose lifetime is independent of any one service.
//
// Runs after instance cleanup so workloads cannot be re-attached to a
// volume that is about to be reclaimed, and before service deregister
// so the OwnerService back-pointer is still resolvable.
type VolumeCleanupFinalizer struct {
	*BaseFinalizer
	store  store.Store
	logger log.Logger
}

// NewVolumeCleanupFinalizer creates a new volume cleanup finalizer.
func NewVolumeCleanupFinalizer(store store.Store, logger log.Logger) *VolumeCleanupFinalizer {
	return &VolumeCleanupFinalizer{
		BaseFinalizer: NewBaseFinalizer(
			types.FinalizerTypeVolumeCleanup,
			[]types.FinalizerDependency{
				{
					DependsOn: types.FinalizerTypeInstanceCleanup,
					Required:  true,
				},
			},
		),
		store:  store,
		logger: logger,
	}
}

// Execute walks volumes in the service's namespace and:
//  1. Clears BoundNode + BoundClaim on every volume claimed by an
//     instance of this service. The agent's volumes Subsystem
//     observes the update via its watch, shouldMount flips to false,
//     and tearDown (Unmount + Detach) fires. For cloud-driver
//     volumes with reclaimPolicy:retain this is the only step that
//     happens — the Volume row stays, the operator manages the
//     backing-store lifecycle out-of-band.
//  2. For volumes the service OWNS (claimTemplate), also deletes the
//     Volume row. The VolumeController's WatchEventDeleted handler
//     then runs the per-volume reclaim policy.
//
// Step 1 runs even for non-owned (operator-managed `claim:`) volumes
// so an in-place service teardown doesn't leave a stranded
// BoundClaim pointing at a Deleted instance — which would otherwise
// require an operator to `rune volume detach --force` before the
// volume could be reused.
func (f *VolumeCleanupFinalizer) Execute(ctx context.Context, service *types.Service) error {
	owner := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
	f.logger.Info("Starting volume cleanup",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace),
		log.Str("owner", owner))

	var vols []types.Volume
	if err := f.store.List(ctx, types.ResourceTypeVolume, service.Namespace, &vols); err != nil {
		return fmt.Errorf("list volumes in namespace %q: %w", service.Namespace, err)
	}

	// Build the set of instance names that belonged to this service,
	// so we can recognise volumes whose BoundClaim still points at
	// one of them. (InstanceCleanupFinalizer ran first and Marked
	// them Deleted but didn't yet clear their volume claims — that's
	// our job.)
	instanceNames := make(map[string]struct{})
	var instances []types.Instance
	if err := f.store.List(ctx, types.ResourceTypeInstance, service.Namespace, &instances); err == nil {
		for _, ins := range instances {
			if ins.ServiceName == service.Name {
				instanceNames[service.Namespace+"/"+ins.Name] = struct{}{}
			}
		}
	}

	unbound := 0
	deleted := 0
	for i := range vols {
		v := &vols[i]

		// Step 1: clear bind state if this volume is claimed by one
		// of our (now-Deleted) instances.
		if _, claimed := instanceNames[v.BoundClaim]; claimed || v.OwnerService == owner {
			if v.BoundNode != "" || v.BoundClaim != "" {
				v.BoundNode = ""
				v.BoundClaim = ""
				if v.Handle != "" {
					v.Status = types.VolumeStatusAvailable
				}
				if err := f.store.Update(ctx, types.ResourceTypeVolume, v.Namespace, v.Name, v); err != nil {
					f.logger.Warn("Failed to release volume bind state on service delete",
						log.Str("volume", v.Name),
						log.Str("namespace", v.Namespace),
						log.Err(err))
					// Continue — best-effort. The volume row will be
					// re-checked on the next operator action.
				} else {
					unbound++
				}
			}
		}

		// Step 2: delete owned (claimTemplate) volume rows.
		if v.OwnerService != owner {
			continue
		}
		if err := f.store.Delete(ctx, types.ResourceTypeVolume, v.Namespace, v.Name); err != nil {
			// Best-effort: log and continue so a single stuck volume
			// does not block service deletion. The operator can
			// re-run with --force or delete the row by hand.
			f.logger.Warn("Failed to delete owned volume",
				log.Str("volume", v.Name),
				log.Str("namespace", v.Namespace),
				log.Err(err))
			continue
		}
		deleted++
	}

	f.logger.Info("Volume cleanup completed",
		log.Str("service", service.Name),
		log.Str("namespace", service.Namespace),
		log.Int("unbound", unbound),
		log.Int("deleted", deleted))
	return nil
}

// Validate checks if the finalizer can be executed.
func (f *VolumeCleanupFinalizer) Validate(service *types.Service) error {
	if service == nil {
		return fmt.Errorf("service cannot be nil")
	}
	return nil
}
