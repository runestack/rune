package releasectl

import (
	"context"
	"fmt"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/types"
)

// UninstallOptions tune an uninstall (Decision D4).
type UninstallOptions struct {
	// KeepVolumes overrides reclaim policy to retain volume data on uninstall.
	KeepVolumes bool
	// Purge removes the release record entirely instead of soft-marking it
	// uninstalled (the default is a soft tombstone, Decision D4).
	Purge bool
}

// Plan computes the 3-way reconcile plan for a spec WITHOUT applying it — the
// dry-run / diff path behind ReleaseService.Plan. It needs only the desired
// resource refs (not the rendered payloads), so callers can render-lite.
func (c *Controller) Plan(ctx context.Context, spec release.ReleaseSpec) (*release.Plan, error) {
	current, found, err := (&records{repo: c.releases}).Get(ctx, spec.Namespace, spec.Name)
	if err != nil {
		return nil, err
	}
	var prev *types.Release
	if found {
		prev = current
	}
	return release.BuildPlan(spec.Name, spec.Namespace, spec.Resources, prev, &liveLookup{c: c}, spec.Options)
}

// Uninstall removes the resources owned by a release in reverse dependency
// order (services → secrets → configmaps → volumes) via the existing prune
// path, then records the outcome.
//
// Soft by default (Decision D4): on success the record is marked uninstalled
// and kept as a tombstone for history/reinstall. opts.Purge hard-deletes the
// record instead. Shared/referenced cluster kinds (StorageClass, Namespace)
// are never deleted (Decision D2) — only the Owns set is pruned.
//
// TODO: honor reclaim policy and opts.KeepVolumes in the prune path
// (currently the applier's volume Prune deletes the record; retain → release
// is a deeper change tracked alongside the DeletionOperation reclaim work).
func (c *Controller) Uninstall(ctx context.Context, namespace, name string, opts UninstallOptions) error {
	rel, err := c.releases.GetByName(ctx, namespace, name)
	if err != nil {
		return fmt.Errorf("release %s/%s not found: %w", namespace, name, err)
	}

	// Mark uninstalling so a crash mid-prune is diagnosable.
	if err := c.releases.SetStatus(ctx, namespace, name, types.ReleaseStatusUninstalling); err != nil {
		c.log.Warn("failed to set uninstalling status", log.Str("release", name), log.Err(err))
	}

	// Prune in reverse dependency order: services first, volumes last. We reuse
	// the same applier prune path the reconcile loop uses.
	a := &applier{c: c}
	for _, ref := range reverseDependencyOrder(rel.Owns) {
		if opts.KeepVolumes && ref.ResourceType == types.ResourceTypeVolume {
			c.log.Info("keeping volume on uninstall", log.Str("volume", ref.Key()))
			continue
		}
		if err := a.Prune(ctx, ref); err != nil {
			// Leave the release in uninstalling so the operator can retry; surface
			// the first failure.
			return fmt.Errorf("prune %s during uninstall: %w", ref.Key(), err)
		}
	}

	if opts.Purge {
		return c.releases.Delete(ctx, namespace, name)
	}

	// Soft tombstone: clear the owned set (everything was pruned) and mark
	// uninstalled, retaining the record for history/reinstall (Decision D4).
	rel.Owns = nil
	rel.Status = types.ReleaseStatusUninstalled
	return c.releases.Update(ctx, rel)
}

// reverseDependencyOrder returns the owned refs ordered for deletion: the
// reverse of the apply rank (services before configmaps/secrets before
// volumes), matching the reconcile prune ordering.
func reverseDependencyOrder(refs []types.OwnerRef) []types.OwnerRef {
	out := make([]types.OwnerRef, len(refs))
	copy(out, refs)
	rank := func(rt types.ResourceType) int {
		switch rt {
		case types.ResourceTypeService:
			return 0
		case types.ResourceTypeSecret:
			return 1
		case types.ResourceTypeConfigmap:
			return 2
		case types.ResourceTypeVolume:
			return 3
		default:
			return 4
		}
	}
	// Stable insertion sort keeps it dependency-free and deterministic.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && rank(out[j].ResourceType) < rank(out[j-1].ResourceType); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
