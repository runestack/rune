// Package releasectl is the server-side executor for stateful runeset releases.
// It wires the pure planning/reconcile core (pkg/release) to the real cluster:
// the orchestrator for services and the store repos for secrets, configmaps and
// volumes. This is the body behind the future ReleaseService.Cast RPC (C1).
//
// Design: _docs/plugins/RUNESET_STATEFUL_RELEASES.md, CAST_REFACTOR_PLAN.md.
package releasectl

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/runestack/rune/pkg/authz"
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

	// admission gates payload-shaped authorization (services.privileged).
	// The orchestrator enforces the same gate, but a release must be
	// admitted BEFORE anything is applied — otherwise a denial lands
	// mid-apply and has to be rolled back, and on the --detach path it
	// lands in a background goroutine nobody is watching. Nil disables.
	admission *authz.Gate
}

// SetAdmission installs the payload-admission gate. Wired by the API
// server when authentication is on; nil leaves admission off.
func (c *Controller) SetAdmission(gate *authz.Gate) { c.admission = gate }

// admit checks every service payload in the release against the
// admission gate, using the caller's context — the only place in the
// cast path where the authenticated subject is guaranteed present.
func (c *Controller) admit(ctx context.Context, p Payloads) error {
	for _, key := range sortedServiceKeys(p.Services) {
		if err := c.admission.AdmitService(ctx, p.Services[key]); err != nil {
			return err
		}
	}
	return nil
}

// sortedServiceKeys gives admission a deterministic order so a release
// with several offending services always reports the same one.
func sortedServiceKeys(m map[string]*types.Service) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// NewController builds a Controller from the orchestrator and store.
func NewController(orch orchestrator.Orchestrator, st store.Store, logger log.Logger) *Controller {
	return &Controller{
		orch:     orch,
		secrets:  repos.NewSecretRepo(st),
		configs:  repos.NewConfigRepo(st),
		volumes:  repos.NewVolumeRepo(st),
		releases: repos.NewReleaseRepo(st),
		log:      logger.WithComponent("release-controller"),
		// Must exceed the update stall deadline (types.UpdateStallSeconds,
		// 600s): a rolling update is slower than the old take-everything-down
		// deploy, and at 5 minutes `--atomic` would time out and revert a
		// merely-slow update before stall detection ever declared one
		// (RUNE-042 §8.3). 15m leaves margin for a slow image pull on top.
		verifyTimeout: 15 * time.Minute,
	}
}

// Cast runs a full reconcile for the given spec + rendered payloads.
func (c *Controller) Cast(ctx context.Context, spec release.ReleaseSpec, p Payloads) (*types.Release, *release.Plan, error) {
	// Admit before planning: nothing in this release reaches the store
	// unless every payload passes.
	if err := c.admit(ctx, p); err != nil {
		return nil, nil, err
	}

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
		// The request context dies with Cast, taking the authenticated
		// subject with it. admit() already ran synchronously above, over
		// this exact payload set, so mark the detached apply as an
		// already-admitted system write rather than letting the
		// orchestrator gate deny it for having no subject.
		if _, e := prep.Execute(authz.WithSystem(context.Background()), a); e != nil {
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

	// pre holds the pre-image of every resource this cast has touched, keyed
	// by OwnerRef.Key(), captured on the read-before-write each Apply/Prune
	// already performs. Existed=false records "was absent". Atomic rollback
	// (Revert) restores from here; the maps live only for the cast's lifetime.
	pre map[string]preImage
}

// preImage is a resource's state before this cast first touched it. Exactly
// one pointer is set when Existed is true; all are nil when it was absent.
type preImage struct {
	Existed   bool
	Service   *types.Service
	Secret    *types.Secret
	Configmap *types.Configmap
	Volume    *types.Volume
}

// capture records ref's pre-image once (first touch wins, so a later Prune of
// a ref the cast already updated can't overwrite the true pre-cast state).
func (a *applier) capture(ref types.OwnerRef, img preImage) {
	if a.pre == nil {
		a.pre = map[string]preImage{}
	}
	if _, ok := a.pre[ref.Key()]; ok {
		return
	}
	a.pre[ref.Key()] = img
}

// deepCopy round-trips a resource through JSON so the captured pre-image can't
// alias state the orchestrator or repos mutate later. These types already
// round-trip through JSON for storage, so the copy is faithful.
func deepCopy[T any](v *T) *T {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	out := new(T)
	if err := json.Unmarshal(b, out); err != nil {
		return nil
	}
	return out
}

// copySecret deep-copies a secret INCLUDING Data, which is json:"-" (stored
// encrypted separately) and would otherwise be dropped by the JSON round-trip —
// a revert restoring a Data-less pre-image would wipe the secret's values.
func copySecret(s *types.Secret) *types.Secret {
	out := deepCopy(s)
	if out != nil && s.Data != nil {
		data := make(map[string]string, len(s.Data))
		for k, v := range s.Data {
			data[k] = v
		}
		out.Data = data
	}
	return out
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
		if existing, err := a.c.orch.GetService(ctx, ref.Namespace, ref.Name); err == nil {
			a.capture(ref, preImage{Existed: true, Service: deepCopy(existing)})
			return a.c.orch.UpdateService(ctx, svc)
		}
		a.capture(ref, preImage{})
		return a.c.orch.CreateService(ctx, svc)
	case types.ResourceTypeSecret:
		s := a.p.Secrets[ref.Key()]
		if s == nil {
			return fmt.Errorf("no rendered payload for secret %s", ref.Key())
		}
		stamp := a.stamp
		s.OwnedBy = &stamp
		if existing, err := a.c.secrets.Get(ctx, ref.Namespace, ref.Name); err == nil {
			a.capture(ref, preImage{Existed: true, Secret: copySecret(existing)})
			return a.c.secrets.Update(ctx, ref.Namespace, ref.Name, s)
		}
		a.capture(ref, preImage{})
		return a.c.secrets.Create(ctx, s)
	case types.ResourceTypeConfigmap:
		cm := a.p.Configmaps[ref.Key()]
		if cm == nil {
			return fmt.Errorf("no rendered payload for configmap %s", ref.Key())
		}
		stamp := a.stamp
		cm.OwnedBy = &stamp
		if existing, err := a.c.configs.Get(ctx, ref.Namespace, ref.Name); err == nil {
			a.capture(ref, preImage{Existed: true, Configmap: deepCopy(existing)})
			return a.c.configs.Update(ctx, ref.Namespace, ref.Name, cm)
		}
		a.capture(ref, preImage{})
		return a.c.configs.Create(ctx, cmRef(ref), cm)
	case types.ResourceTypeVolume:
		v := a.p.Volumes[ref.Key()]
		if v == nil {
			return fmt.Errorf("no rendered payload for volume %s", ref.Key())
		}
		stamp := a.stamp
		v.OwnedBy = &stamp
		if existing, err := a.c.volumes.Get(ctx, ref.Namespace, ref.Name); err == nil {
			a.capture(ref, preImage{Existed: true, Volume: deepCopy(existing)})
			return a.c.volumes.Update(ctx, v)
		}
		a.capture(ref, preImage{})
		return a.c.volumes.Create(ctx, v)
	default:
		// ActionReference for shared cluster kinds (Namespace, StorageClass):
		// ensure-exists is handled by the existing cast/create-namespace paths.
		// TODO: ensure the referenced resource exists and record the dependency.
		return nil
	}
}

func (a *applier) Prune(ctx context.Context, ref types.OwnerRef) error {
	// Capture the pre-image first so an atomic rollback can restore a pruned
	// resource (capture is first-touch-wins, so this never clobbers a pre-image
	// taken before an earlier Apply of the same ref).
	a.capturePreDelete(ctx, ref)

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

// capturePreDelete snapshots ref's current state ahead of a Prune. Best-effort:
// a lookup miss records "absent", which makes the eventual Revert a no-op.
func (a *applier) capturePreDelete(ctx context.Context, ref types.OwnerRef) {
	switch ref.ResourceType {
	case types.ResourceTypeService:
		if existing, err := a.c.orch.GetService(ctx, ref.Namespace, ref.Name); err == nil {
			a.capture(ref, preImage{Existed: true, Service: deepCopy(existing)})
			return
		}
	case types.ResourceTypeSecret:
		if existing, err := a.c.secrets.Get(ctx, ref.Namespace, ref.Name); err == nil {
			a.capture(ref, preImage{Existed: true, Secret: copySecret(existing)})
			return
		}
	case types.ResourceTypeConfigmap:
		if existing, err := a.c.configs.Get(ctx, ref.Namespace, ref.Name); err == nil {
			a.capture(ref, preImage{Existed: true, Configmap: deepCopy(existing)})
			return
		}
	case types.ResourceTypeVolume:
		if existing, err := a.c.volumes.Get(ctx, ref.Namespace, ref.Name); err == nil {
			a.capture(ref, preImage{Existed: true, Volume: deepCopy(existing)})
			return
		}
	default:
		return
	}
	a.capture(ref, preImage{})
}

// Revert restores ref to its captured pre-image (atomic rollback): absent
// before the cast → delete-if-exists; present before → restore that state via
// update-or-create. Tolerates the original change having never landed.
// Revert restores state the cluster already accepted, so it runs as a
// system write: a rollback must never be blocked by a verb the
// rolling-back subject lacks, or a failed cast would strand the cluster
// half-applied.
func (a *applier) Revert(ctx context.Context, ref types.OwnerRef) error {
	ctx = authz.WithSystem(ctx)
	pi, ok := a.pre[ref.Key()]
	if !ok {
		// No pre-image means the change never reached a mutation: capture
		// strictly precedes every Create/Update/Delete, so a change that failed
		// before touching the cluster (e.g. a missing payload) has nothing to
		// revert.
		return nil
	}

	if !pi.Existed {
		// Created (or attempted) by this cast: remove it if it landed.
		switch ref.ResourceType {
		case types.ResourceTypeService:
			if _, err := a.c.orch.GetService(ctx, ref.Namespace, ref.Name); err != nil {
				return nil // never landed
			}
			_, err := a.c.orch.DeleteService(ctx, &types.DeletionRequest{
				Namespace: ref.Namespace,
				Name:      ref.Name,
			})
			return err
		case types.ResourceTypeSecret:
			if _, err := a.c.secrets.Get(ctx, ref.Namespace, ref.Name); err != nil {
				return nil
			}
			return a.c.secrets.Delete(ctx, ref.Namespace, ref.Name)
		case types.ResourceTypeConfigmap:
			if _, err := a.c.configs.Get(ctx, ref.Namespace, ref.Name); err != nil {
				return nil
			}
			return a.c.configs.Delete(ctx, ref.Namespace, ref.Name)
		case types.ResourceTypeVolume:
			if _, err := a.c.volumes.Get(ctx, ref.Namespace, ref.Name); err != nil {
				return nil
			}
			return a.c.volumes.Delete(ctx, ref.Namespace, ref.Name)
		default:
			return nil
		}
	}

	// Existed before the cast: restore the captured state (with its original
	// OwnedBy stamp) — update if still present, recreate if pruned/deleted.
	switch ref.ResourceType {
	case types.ResourceTypeService:
		if pi.Service == nil {
			return fmt.Errorf("pre-image for %s lost", ref.Key())
		}
		if _, err := a.c.orch.GetService(ctx, ref.Namespace, ref.Name); err == nil {
			return a.c.orch.UpdateService(ctx, pi.Service)
		}
		return a.c.orch.CreateService(ctx, pi.Service)
	case types.ResourceTypeSecret:
		if pi.Secret == nil {
			return fmt.Errorf("pre-image for %s lost", ref.Key())
		}
		if _, err := a.c.secrets.Get(ctx, ref.Namespace, ref.Name); err == nil {
			return a.c.secrets.Update(ctx, ref.Namespace, ref.Name, pi.Secret)
		}
		return a.c.secrets.Create(ctx, pi.Secret)
	case types.ResourceTypeConfigmap:
		if pi.Configmap == nil {
			return fmt.Errorf("pre-image for %s lost", ref.Key())
		}
		if _, err := a.c.configs.Get(ctx, ref.Namespace, ref.Name); err == nil {
			return a.c.configs.Update(ctx, ref.Namespace, ref.Name, pi.Configmap)
		}
		return a.c.configs.Create(ctx, cmRef(ref), pi.Configmap)
	case types.ResourceTypeVolume:
		if pi.Volume == nil {
			return fmt.Errorf("pre-image for %s lost", ref.Key())
		}
		if _, err := a.c.volumes.Get(ctx, ref.Namespace, ref.Name); err == nil {
			return a.c.volumes.Update(ctx, pi.Volume)
		}
		return a.c.volumes.Create(ctx, pi.Volume)
	default:
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
