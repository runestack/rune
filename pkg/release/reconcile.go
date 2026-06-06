package release

import (
	"context"
	"errors"
	"sort"

	"github.com/runestack/rune/pkg/types"
)

// ErrConflicts is returned when a plan cannot be applied because it contains
// unresolved ownership conflicts (resource owned elsewhere, no --adopt).
var ErrConflicts = errors.New("release plan has unresolved conflicts; pass --adopt or resolve ownership")

// ErrDetachWouldPrune is returned when --detach is requested for a plan that
// would delete resources (Decision C3: detach is create/update-only).
var ErrDetachWouldPrune = errors.New("--detach is not allowed for a plan that prunes resources")

// ReleaseSpec is the rendered, ready-to-apply description of a release. It is
// produced client-side (resolve → render → lint) and executed by the
// server-side reconciler. Resource payloads are carried by the Applier
// implementation, keyed by ref; this spec holds identity, provenance, and the
// desired ref set.
type ReleaseSpec struct {
	Name           string
	Namespace      string
	Source         types.ReleaseSource
	Manifest       types.RunesetManifest
	Values         map[string]interface{}
	RenderedDigest string
	Resources      []DesiredResource
	Options        Options

	// Detach requests create/update-only async completion (Decision C3). A
	// detach spec whose plan prunes is rejected with ErrDetachWouldPrune.
	Detach bool
}

// Applier executes individual plan actions against the cluster. Implemented
// server-side against the orchestrator + repos; faked in tests.
type Applier interface {
	// Apply materializes a create / update / adopt / reference change.
	Apply(ctx context.Context, change PlannedChange) error
	// Prune deletes a no-longer-desired owned resource, honoring reclaim policy.
	Prune(ctx context.Context, ref types.OwnerRef) error
	// Verify blocks until the given owned resources are healthy (or errors).
	Verify(ctx context.Context, refs []types.OwnerRef) error
}

// ReleaseRecords is the persistence the reconciler needs (satisfied by
// ReleaseRepo). Get returns found=false when no record exists yet.
type ReleaseRecords interface {
	Get(ctx context.Context, namespace, name string) (rel *types.Release, found bool, err error)
	Save(ctx context.Context, rel *types.Release) error
}

// Reconcile executes a release spec end to end:
//
//	plan → write pending intent → apply (ordered) → verify → prune-last → deployed
//
// On apply/verify failure it records the release as failed and returns; nothing
// is pruned (Decision D3: prune only after a clean apply). The previous revision
// remains recoverable from store history.
func Reconcile(ctx context.Context, spec ReleaseSpec, recs ReleaseRecords, live LiveLookup, applier Applier) (*types.Release, *Plan, error) {
	current, found, err := recs.Get(ctx, spec.Namespace, spec.Name)
	if err != nil {
		return nil, nil, err
	}
	var prev *types.Release
	if found {
		prev = current
	}

	plan, err := BuildPlan(spec.Name, spec.Namespace, spec.Resources, prev, live, spec.Options)
	if err != nil {
		return nil, nil, err
	}
	if !plan.Applyable() {
		return nil, plan, ErrConflicts
	}
	if spec.Detach && plan.HasPrune() {
		return nil, plan, ErrDetachWouldPrune
	}

	revision := 1
	if prev != nil {
		revision = prev.Revision + 1
	}

	owns, refs := partitionOwnership(plan)

	// 1) Record intent up front so a crash is recoverable.
	rel := &types.Release{
		Name:           spec.Name,
		Namespace:      spec.Namespace,
		Status:         types.ReleaseStatusPending,
		Revision:       revision,
		Source:         spec.Source,
		Manifest:       spec.Manifest,
		Values:         spec.Values,
		RenderedDigest: spec.RenderedDigest,
		Owns:           owns,
		References:     refs,
	}
	if found && current.ID != "" {
		rel.ID = current.ID
	}
	if err := recs.Save(ctx, rel); err != nil {
		return nil, plan, err
	}

	applies, prunes := splitForExecution(plan)

	// 2) Apply create/update/adopt/reference in dependency order.
	for _, change := range applies {
		if err := applier.Apply(ctx, change); err != nil {
			return failRelease(ctx, recs, rel, err)
		}
	}

	// 3) Verify owned resources are healthy before any destructive step.
	if err := applier.Verify(ctx, owns); err != nil {
		return failRelease(ctx, recs, rel, err)
	}

	// 4) Prune last (never reached on a failed apply/verify).
	for _, change := range prunes {
		if err := applier.Prune(ctx, change.Ref); err != nil {
			return failRelease(ctx, recs, rel, err)
		}
	}

	// 5) Mark deployed.
	rel.Status = types.ReleaseStatusDeployed
	if err := recs.Save(ctx, rel); err != nil {
		return nil, plan, err
	}
	return rel, plan, nil
}

func failRelease(ctx context.Context, recs ReleaseRecords, rel *types.Release, cause error) (*types.Release, *Plan, error) {
	rel.Status = types.ReleaseStatusFailed
	// Best-effort persist of the failed status; surface the original cause.
	_ = recs.Save(ctx, rel)
	return rel, nil, cause
}

// partitionOwnership splits a plan's resources into owned (create/update/adopt)
// vs referenced (shared cluster kinds). Prunes are excluded from both.
func partitionOwnership(p *Plan) (owns, refs []types.OwnerRef) {
	for i := range p.Changes {
		switch p.Changes[i].Action {
		case ActionCreate, ActionUpdate, ActionAdopt:
			owns = append(owns, p.Changes[i].Ref)
		case ActionReference:
			refs = append(refs, p.Changes[i].Ref)
		}
	}
	return owns, refs
}

// splitForExecution separates apply-phase changes from prune-phase changes and
// orders each deterministically: applies in dependency order (namespace →
// storageclass → volume → configmap → secret → service), prunes in reverse.
func splitForExecution(p *Plan) (applies, prunes []PlannedChange) {
	for i := range p.Changes {
		if p.Changes[i].Action == ActionPrune {
			prunes = append(prunes, p.Changes[i])
		} else {
			applies = append(applies, p.Changes[i])
		}
	}
	sort.SliceStable(applies, func(i, j int) bool {
		ri, rj := applyRank(applies[i].Ref.ResourceType), applyRank(applies[j].Ref.ResourceType)
		if ri != rj {
			return ri < rj
		}
		return applies[i].Ref.Key() < applies[j].Ref.Key()
	})
	sort.SliceStable(prunes, func(i, j int) bool {
		ri, rj := applyRank(prunes[i].Ref.ResourceType), applyRank(prunes[j].Ref.ResourceType)
		if ri != rj {
			return ri > rj // reverse dependency order for deletion
		}
		return prunes[i].Ref.Key() < prunes[j].Ref.Key()
	})
	return applies, prunes
}

// applyRank gives the dependency rank for ordering applies (lower = earlier).
func applyRank(rt types.ResourceType) int {
	switch rt {
	case types.ResourceTypeNamespace:
		return 0
	case types.ResourceTypeStorageClass:
		return 1
	case types.ResourceTypeVolume:
		return 2
	case types.ResourceTypeConfigmap:
		return 3
	case types.ResourceTypeSecret:
		return 4
	case types.ResourceTypeService:
		return 5
	default:
		return 6
	}
}
