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

// Execute deletes all Volume rows owned by the given service.
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

	deleted := 0
	for i := range vols {
		v := &vols[i]
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
