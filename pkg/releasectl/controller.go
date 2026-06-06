// Package releasectl is the server-side executor for stateful runeset releases.
// It wires the pure planning/reconcile core (pkg/release) to the real cluster:
// the orchestrator for services and the store repos for secrets, configmaps and
// volumes. This is the body behind the future ReleaseService.Cast RPC (C1).
//
// Design: _docs/plugins/RUNESET_STATEFUL_RELEASES.md, CAST_REFACTOR_PLAN.md.
package releasectl

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

// Payloads carries the rendered resource objects for a cast, keyed by
// OwnerRef.Key(). The CLI renders the castfile set into these before calling
// Cast; the executor stamps ownership and applies them.
type Payloads struct {
	Services   map[string]*types.Service
	Secrets    map[string]*types.Secret
	Configmaps map[string]*types.Configmap
	Volumes    map[string]*types.Volume
}

// Controller executes release specs against the cluster.
type Controller struct {
	orch     orchestrator.Orchestrator
	secrets  *repos.SecretRepo
	configs  *repos.ConfigmapRepo
	volumes  *repos.VolumeRepo
	releases *repos.ReleaseRepo
	log      log.Logger

	// verifyTimeout bounds how long Verify waits for owned services to become
	// Running (aggregate, per release — fixes today's per-service N×timeout).
	verifyTimeout time.Duration
}

// NewController builds a Controller from the orchestrator and store.
func NewController(orch orchestrator.Orchestrator, st store.Store, logger log.Logger) *Controller {
	return &Controller{
		orch:          orch,
		secrets:       repos.NewSecretRepo(st),
		configs:       repos.NewConfigRepo(st),
		volumes:       repos.NewVolumeRepo(st),
		releases:      repos.NewReleaseRepo(st),
		log:           logger.WithComponent("release-controller"),
		verifyTimeout: 5 * time.Minute,
	}
}

// Cast runs a full reconcile for the given spec + rendered payloads.
func (c *Controller) Cast(ctx context.Context, spec release.ReleaseSpec, p Payloads) (*types.Release, *release.Plan, error) {
	// Precompute the revision so the OwnedBy stamp matches the record Reconcile
	// will write (Reconcile derives the same value independently).
	revision := 1
	if cur, err := c.releases.GetByName(ctx, spec.Namespace, spec.Name); err == nil && cur != nil {
		revision = cur.Revision + 1
	}

	a := &applier{
		c: c,
		p: p,
		stamp: types.OwnedBy{
			Release:  spec.Name,
			Revision: revision,
			Manager:  types.ManagerRuneset,
		},
	}
	recs := &records{repo: c.releases}
	live := &liveLookup{c: c}

	if !spec.Detach {
		return release.Reconcile(ctx, spec, recs, live, a)
	}

	// Detach (C3-a): plan + record the pending intent synchronously, then hand
	// the apply→verify reconcile to a background goroutine with a DETACHED
	// context (the request ctx is cancelled once Cast returns), and return the
	// pending release immediately. Prepare already rejects a detach plan that
	// prunes (ErrDetachWouldPrune), so the background half is never destructive.
	prep, err := release.Prepare(ctx, spec, recs, live)
	if err != nil {
		if prep != nil {
			return nil, prep.Plan, err
		}
		return nil, nil, err
	}
	go func() {
		if _, e := prep.Execute(context.Background(), a); e != nil {
			c.log.Warn("detached release reconcile failed",
				log.Str("release", spec.Name),
				log.Str("namespace", spec.Namespace),
				log.Err(e))
		}
	}()
	return prep.Release, prep.Plan, nil
}

// --- ReleaseRecords adapter over ReleaseRepo ---

type records struct{ repo *repos.ReleaseRepo }

func (r *records) Get(ctx context.Context, namespace, name string) (*types.Release, bool, error) {
	rel, err := r.repo.GetByName(ctx, namespace, name)
	if err != nil {
		// TODO: distinguish not-found from transient store errors via a
		// store.IsNotFound sentinel rather than treating all errors as absent.
		return nil, false, nil
	}
	return rel, true, nil
}

func (r *records) Save(ctx context.Context, rel *types.Release) error {
	if _, found, _ := r.Get(ctx, rel.Namespace, rel.Name); found {
		return r.repo.Update(ctx, rel)
	}
	return r.repo.Create(ctx, rel)
}

// --- LiveLookup over the cluster, reading the OwnedBy stamp ---

type liveLookup struct{ c *Controller }

func (l *liveLookup) Lookup(ref types.OwnerRef) (release.LiveState, error) {
	switch ref.ResourceType {
	case types.ResourceTypeService:
		svc, err := l.c.orch.GetService(context.Background(), ref.Namespace, ref.Name)
		if err != nil {
			return release.LiveState{Exists: false}, nil
		}
		var ob *types.OwnedBy
		if svc.Metadata != nil {
			ob = svc.Metadata.OwnedBy
		}
		return release.LiveState{Exists: true, OwnedBy: ob}, nil
	case types.ResourceTypeSecret:
		s, err := l.c.secrets.Get(context.Background(), ref.Namespace, ref.Name)
		if err != nil {
			return release.LiveState{Exists: false}, nil
		}
		return release.LiveState{Exists: true, OwnedBy: s.OwnedBy}, nil
	case types.ResourceTypeConfigmap:
		cm, err := l.c.configs.Get(context.Background(), ref.Namespace, ref.Name)
		if err != nil {
			return release.LiveState{Exists: false}, nil
		}
		return release.LiveState{Exists: true, OwnedBy: cm.OwnedBy}, nil
	case types.ResourceTypeVolume:
		v, err := l.c.volumes.Get(context.Background(), ref.Namespace, ref.Name)
		if err != nil {
			return release.LiveState{Exists: false}, nil
		}
		return release.LiveState{Exists: true, OwnedBy: v.OwnedBy}, nil
	default:
		// Shared cluster kinds are planned as ActionReference and never reach a
		// live lookup; anything else is treated as absent.
		return release.LiveState{Exists: false}, nil
	}
}

// --- Applier: materialize/prune/verify against the cluster ---

type applier struct {
	c     *Controller
	p     Payloads
	stamp types.OwnedBy
}

func (a *applier) Apply(ctx context.Context, change release.PlannedChange) error {
	ref := change.Ref
	switch ref.ResourceType {
	case types.ResourceTypeService:
		svc := a.p.Services[ref.Key()]
		if svc == nil {
			return fmt.Errorf("no rendered payload for service %s", ref.Key())
		}
		if svc.Metadata == nil {
			svc.Metadata = &types.ServiceMetadata{}
		}
		stamp := a.stamp
		svc.Metadata.OwnedBy = &stamp
		if _, err := a.c.orch.GetService(ctx, ref.Namespace, ref.Name); err == nil {
			return a.c.orch.UpdateService(ctx, svc)
		}
		return a.c.orch.CreateService(ctx, svc)
	case types.ResourceTypeSecret:
		s := a.p.Secrets[ref.Key()]
		if s == nil {
			return fmt.Errorf("no rendered payload for secret %s", ref.Key())
		}
		stamp := a.stamp
		s.OwnedBy = &stamp
		if _, err := a.c.secrets.Get(ctx, ref.Namespace, ref.Name); err == nil {
			return a.c.secrets.Update(ctx, ref.Namespace, ref.Name, s)
		}
		return a.c.secrets.Create(ctx, s)
	case types.ResourceTypeConfigmap:
		cm := a.p.Configmaps[ref.Key()]
		if cm == nil {
			return fmt.Errorf("no rendered payload for configmap %s", ref.Key())
		}
		stamp := a.stamp
		cm.OwnedBy = &stamp
		if _, err := a.c.configs.Get(ctx, ref.Namespace, ref.Name); err == nil {
			return a.c.configs.Update(ctx, ref.Namespace, ref.Name, cm)
		}
		return a.c.configs.Create(ctx, cmRef(ref), cm)
	case types.ResourceTypeVolume:
		v := a.p.Volumes[ref.Key()]
		if v == nil {
			return fmt.Errorf("no rendered payload for volume %s", ref.Key())
		}
		stamp := a.stamp
		v.OwnedBy = &stamp
		if _, err := a.c.volumes.Get(ctx, ref.Namespace, ref.Name); err == nil {
			return a.c.volumes.Update(ctx, v)
		}
		return a.c.volumes.Create(ctx, v)
	default:
		// ActionReference for shared cluster kinds (Namespace, StorageClass):
		// ensure-exists is handled by the existing cast/create-namespace paths.
		// TODO: ensure the referenced resource exists and record the dependency.
		return nil
	}
}

func (a *applier) Prune(ctx context.Context, ref types.OwnerRef) error {
	switch ref.ResourceType {
	case types.ResourceTypeService:
		// TODO: honor grace period / finalizers via DeletionRequest options.
		_, err := a.c.orch.DeleteService(ctx, &types.DeletionRequest{
			Namespace: ref.Namespace,
			Name:      ref.Name,
		})
		return err
	case types.ResourceTypeSecret:
		return a.c.secrets.Delete(ctx, ref.Namespace, ref.Name)
	case types.ResourceTypeConfigmap:
		return a.c.configs.Delete(ctx, ref.Namespace, ref.Name)
	case types.ResourceTypeVolume:
		// TODO: honor reclaim policy (retain → release, not destroy) and
		// --keep-volumes before deleting the volume record.
		return a.c.volumes.Delete(ctx, ref.Namespace, ref.Name)
	default:
		// Shared cluster kinds are referenced, never pruned (Decision D2).
		return nil
	}
}

func (a *applier) Verify(ctx context.Context, refs []types.OwnerRef) error {
	// Only services have a readiness notion to wait on; other kinds are durable
	// the moment they're written.
	var services []types.OwnerRef
	for _, r := range refs {
		if r.ResourceType == types.ResourceTypeService {
			services = append(services, r)
		}
	}
	if len(services) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, a.c.verifyTimeout)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		allReady := true
		for _, r := range services {
			st, err := a.c.orch.GetServiceStatus(ctx, r.Namespace, r.Name)
			if err != nil {
				allReady = false
				break
			}
			if st.Status == types.ServiceStatusFailed {
				return fmt.Errorf("service %s entered Failed during release verify", r.Key())
			}
			if st.Status != types.ServiceStatusRunning {
				allReady = false
			}
		}
		if allReady {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("release verify timed out after %s: %w", a.c.verifyTimeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

// cmRef builds the "configmap:name.namespace.rune" ref ConfigmapRepo.Create expects.
func cmRef(ref types.OwnerRef) string {
	return fmt.Sprintf("configmap:%s.%s.rune", ref.Name, ref.Namespace)
}

// compile-time interface checks
var (
	_ release.ReleaseRecords = (*records)(nil)
	_ release.LiveLookup     = (*liveLookup)(nil)
	_ release.Applier        = (*applier)(nil)
)
