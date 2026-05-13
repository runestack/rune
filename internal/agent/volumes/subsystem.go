// Package volumes is the per-node Subsystem that drives the storage
// driver Attach/Mount/Unmount/Detach lifecycle for Volumes whose
// BoundNode equals this node's identity.
//
// It is the agent-side counterpart to pkg/orchestrator/controllers/
// volume_controller.go: the controller decides where a volume is bound
// (and is the only writer of Volume.BoundNode); this subsystem watches
// those bindings and turns them into real node-local mounts under
// `<MountRoot>/<volume.ID>/` (default `/var/lib/rune/mounts`).
//
// Scope intentionally narrow for the first agent-side slice of RUNE-069:
//
//   - Watches ResourceTypeVolume across all namespaces.
//   - For each volume bound to this node (`BoundNode == nodeID`,
//     status in {Available, Bound}, non-empty Handle), calls
//     Driver.Attach then Driver.Mount and records the resulting
//     MountTarget in an in-memory table.
//   - For previously-tracked volumes that no longer match (BoundNode
//     changed, status regressed, deleted), calls Unmount then Detach
//     and drops the entry.
//   - Public accessor MountTargetFor(volumeID) lets the runner-side
//     resolver swap the bare Volume.Handle for the agent-driven mount
//     target once the instance controller is updated to consult it.
//
// Out of scope (deferred to follow-ups):
//
//   - VolumeBound writeback: this Subsystem does not flip status to
//     Bound or write BoundNode itself; that remains the controller's
//     job in a later slice.
//   - Persistence of the mount table across restarts: today every
//     Stop/Start re-reconciles from scratch by walking the watch.
package volumes

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// DefaultMountRoot is the per-node directory under which the subsystem
// nests one subdirectory per Volume.ID. It mirrors the layout called out
// in the RUNE-069 design.
const DefaultMountRoot = "/var/lib/rune/mounts"

// DriverLookup resolves the storage Driver responsible for a given
// Volume. The implementation typically reads the Volume's StorageClass
// from the store to learn the driver name, then asks the driver
// registry for an instance. Keeping that two-step dance behind a
// closure lets this package stay free of any direct store-access for
// driver resolution and trivial to fake in tests.
type DriverLookup func(ctx context.Context, vol *types.Volume) (driver.Driver, error)

// Config bundles construction parameters.
type Config struct {
	// Store is the resource store to watch. Required.
	Store store.Store

	// NodeID is the local node's stable identity. The subsystem only
	// acts on Volumes whose BoundNode matches. Required.
	NodeID string

	// Lookup resolves a Volume to a concrete Driver instance.
	// Required.
	Lookup DriverLookup

	// MountRoot is the per-node directory under which mount targets
	// live. Defaults to DefaultMountRoot.
	MountRoot string

	// Logger; defaults to the global logger with component
	// "agent.volumes".
	Logger log.Logger
}

// Subsystem implements the agent.Subsystem contract:
//
//	Name() string
//	Start(ctx context.Context) error
//	Ready() <-chan struct{}
//	Stop(ctx context.Context) error
//
// The interface itself lives in internal/agent; depending on it here
// would import-cycle, so the contract is honoured structurally.
type Subsystem struct {
	cfg Config
	log log.Logger

	mu      sync.Mutex
	started bool
	stopped bool

	readyCh chan struct{}
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// mounts is the in-memory record of (volumeID -> tracked mount
	// state). Protected by stateMu rather than mu so reads from
	// MountTargetFor don't contend with lifecycle calls.
	stateMu sync.RWMutex
	mounts  map[string]trackedMount
}

// trackedMount is the per-volume bookkeeping the subsystem keeps for
// every volume currently attached + mounted on this node.
type trackedMount struct {
	// Driver is the concrete driver that owns the handle.
	Driver driver.Driver
	// Handle is the opaque driver handle copied from Volume.Handle at
	// mount time.
	Handle driver.VolumeHandle
	// Target is the mount target the driver returned (drivers may
	// rewrite the proposed target).
	Target driver.MountTarget
	// OpCtx is the OpContext used to bring this mount up, retained so
	// the symmetric Unmount/Detach calls during tearDown see the same
	// per-class parameters (region, auth refs, …) the driver was given
	// at Attach/Mount time. Snapshotting here is also defence against
	// the StorageClass being deleted between bringUp and tearDown.
	OpCtx driver.OpContext
	// VolumeNS / VolumeName are kept so logs read sensibly.
	VolumeNS   string
	VolumeName string
}

// New constructs a Subsystem. It does not touch the store or filesystem
// until Start is called.
func New(cfg Config) (*Subsystem, error) {
	if cfg.Store == nil {
		return nil, errors.New("agent.volumes: nil Store")
	}
	if cfg.NodeID == "" {
		return nil, errors.New("agent.volumes: empty NodeID")
	}
	if cfg.Lookup == nil {
		return nil, errors.New("agent.volumes: nil Lookup")
	}
	if cfg.MountRoot == "" {
		cfg.MountRoot = DefaultMountRoot
	}
	if cfg.Logger == nil {
		cfg.Logger = log.GetDefaultLogger().WithComponent("agent.volumes")
	} else {
		cfg.Logger = cfg.Logger.WithComponent("agent.volumes")
	}
	return &Subsystem{
		cfg:     cfg,
		log:     cfg.Logger.With(log.Str("node_id", cfg.NodeID)),
		readyCh: make(chan struct{}),
		mounts:  make(map[string]trackedMount),
	}, nil
}

// Name identifies the subsystem in logs / metrics.
func (s *Subsystem) Name() string { return "agent.volumes" }

// Ready returns a channel closed once the subsystem has opened its
// watch and finished the initial reconcile pass.
func (s *Subsystem) Ready() <-chan struct{} { return s.readyCh }

// Start opens the volume watch and begins reconciling bindings.
// Returns promptly; long-running work runs in a goroutine.
func (s *Subsystem) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("agent.volumes: Start called twice")
	}
	if s.stopped {
		s.mu.Unlock()
		return errors.New("agent.volumes: Start after Stop")
	}
	s.started = true
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	watchCh, err := s.cfg.Store.Watch(runCtx, types.ResourceTypeVolume, "")
	if err != nil {
		cancel()
		return fmt.Errorf("agent.volumes: watch: %w", err)
	}

	if err := s.initialReconcile(runCtx); err != nil {
		s.log.Warn("Initial volume reconcile had errors", log.Err(err))
	}
	close(s.readyCh)

	s.wg.Add(1)
	go s.run(runCtx, watchCh)
	s.log.Info("Volume subsystem started",
		log.Str("mount_root", s.cfg.MountRoot))
	return nil
}

// Stop tears down the watch and unmounts every tracked volume. Safe
// to call after a failed Start.
func (s *Subsystem) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.stopped = true
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.wg.Wait()

	// Drain every tracked mount. Use the caller-supplied ctx so the
	// outer agent shutdown deadline applies.
	s.stateMu.Lock()
	tracked := make(map[string]trackedMount, len(s.mounts))
	for k, v := range s.mounts {
		tracked[k] = v
	}
	s.mounts = make(map[string]trackedMount)
	s.stateMu.Unlock()

	for id, m := range tracked {
		if err := s.tearDown(ctx, id, m); err != nil {
			s.log.Warn("Volume teardown failed during Stop",
				log.Str("volume_id", id),
				log.Str("namespace", m.VolumeNS),
				log.Str("name", m.VolumeName),
				log.Err(err))
		}
	}

	s.log.Info("Volume subsystem stopped")
	return nil
}

// MountTargetFor returns the host path the named volume is currently
// mounted at on this node (or false if the subsystem has not mounted
// it). Called by the instance controller's resolveVolumeMount via the
// MountResolver hook; the controller falls back to Volume.Handle when
// this returns false.
func (s *Subsystem) MountTargetFor(volumeID string) (string, bool) {
	s.stateMu.RLock()
	m, ok := s.mounts[volumeID]
	s.stateMu.RUnlock()
	if !ok {
		return "", false
	}
	return string(m.Target), true
}

// ---------------------------------------------------------------------------
// Reconcile loop
// ---------------------------------------------------------------------------

func (s *Subsystem) run(ctx context.Context, initial <-chan store.WatchEvent) {
	defer s.wg.Done()
	watchCh := initial
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-watchCh:
			if !ok {
				next, err := s.cfg.Store.Watch(ctx, types.ResourceTypeVolume, "")
				if err != nil {
					if errors.Is(ctx.Err(), context.Canceled) {
						return
					}
					s.log.Error("Volume watch restart failed", log.Err(err))
					return
				}
				watchCh = next
				continue
			}
			if err := s.handle(ctx, ev); err != nil {
				s.log.Error("Volume event handling failed",
					log.Str("namespace", ev.Namespace),
					log.Str("name", ev.Name),
					log.Str("type", string(ev.Type)),
					log.Err(err))
			}
		}
	}
}

// initialReconcile walks every existing Volume so that a freshly-started
// subsystem on a node that already has bindings catches up with the
// world before relying on the watch stream.
func (s *Subsystem) initialReconcile(ctx context.Context) error {
	var vols []types.Volume
	if err := s.cfg.Store.ListAll(ctx, types.ResourceTypeVolume, &vols); err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	var firstErr error
	for i := range vols {
		v := vols[i]
		if err := s.reconcile(ctx, &v); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Subsystem) handle(ctx context.Context, ev store.WatchEvent) error {
	switch ev.Type {
	case store.WatchEventCreated, store.WatchEventUpdated:
		vol, ok := ev.Resource.(*types.Volume)
		if !ok {
			return fmt.Errorf("agent.volumes: expected *types.Volume, got %T", ev.Resource)
		}
		return s.reconcile(ctx, vol)
	case store.WatchEventDeleted:
		// Drop any mount tracked for this volume regardless of the
		// event body shape — the volume row is gone, the operator
		// (or VolumeController) is responsible for the backing-store
		// reclaim, but the node-local mount must come down.
		id := ""
		if vol, ok := ev.Resource.(*types.Volume); ok {
			id = vol.ID
		}
		if id == "" {
			// Some backends omit the body; fall back to looking up
			// the tracked entry by ev.Name within ev.Namespace.
			id = s.lookupTrackedID(ev.Namespace, ev.Name)
		}
		if id == "" {
			return nil
		}
		s.stateMu.Lock()
		m, tracked := s.mounts[id]
		if tracked {
			delete(s.mounts, id)
		}
		s.stateMu.Unlock()
		if !tracked {
			return nil
		}
		return s.tearDown(ctx, id, m)
	default:
		return nil
	}
}

// reconcile owns the per-volume decision: should this node have it
// mounted, and is the in-memory tracker in agreement?
func (s *Subsystem) reconcile(ctx context.Context, vol *types.Volume) error {
	id := vol.ID
	if id == "" {
		// Defensive: pre-RUNE-069 rows or test fixtures may not have
		// stamped an ID. Fall back to "<ns>/<name>" so the tracking
		// key is at least stable.
		id = vol.Namespace + "/" + vol.Name
	}

	want := s.shouldMount(vol)

	s.stateMu.RLock()
	tracked, isTracked := s.mounts[id]
	s.stateMu.RUnlock()

	switch {
	case want && !isTracked:
		return s.bringUp(ctx, vol, id)
	case !want && isTracked:
		s.stateMu.Lock()
		delete(s.mounts, id)
		s.stateMu.Unlock()
		return s.tearDown(ctx, id, tracked)
	case want && isTracked:
		// Already mounted. If the handle changed under us (the
		// controller re-provisioned), tear the old mount down and
		// re-mount.
		if tracked.Handle != driver.VolumeHandle(vol.Handle) {
			s.stateMu.Lock()
			delete(s.mounts, id)
			s.stateMu.Unlock()
			if err := s.tearDown(ctx, id, tracked); err != nil {
				s.log.Warn("Tear-down before re-mount failed",
					log.Str("volume_id", id),
					log.Err(err))
			}
			return s.bringUp(ctx, vol, id)
		}
	}
	return nil
}

// shouldMount returns true when the volume should be attached + mounted
// on this node.
func (s *Subsystem) shouldMount(vol *types.Volume) bool {
	if vol == nil {
		return false
	}
	if vol.BoundNode != s.cfg.NodeID {
		return false
	}
	if vol.Handle == "" {
		return false
	}
	switch vol.Status {
	case types.VolumeStatusAvailable, types.VolumeStatusBound:
		return true
	default:
		return false
	}
}

// bringUp resolves the driver, calls Attach + Mount and records the
// result. Errors are logged but not propagated up the watch loop —
// the per-volume retry is handled by the controller (which observes
// the same watch and may flip status), not by us.
func (s *Subsystem) bringUp(ctx context.Context, vol *types.Volume, id string) error {
	if vol.StorageClassName == "" {
		return fmt.Errorf("agent.volumes: volume %s has no storageClassName", vol.String())
	}
	drv, err := s.cfg.Lookup(ctx, vol)
	if err != nil {
		return fmt.Errorf("agent.volumes: lookup driver for %s: %w", vol.String(), err)
	}
	opctx, err := s.buildOpContext(ctx, vol)
	if err != nil {
		return fmt.Errorf("agent.volumes: build OpContext for %s: %w", vol.String(), err)
	}
	handle := driver.VolumeHandle(vol.Handle)
	dev, err := drv.Attach(ctx, opctx, handle, driver.NodeID(s.cfg.NodeID))
	if err != nil {
		return fmt.Errorf("agent.volumes: attach %s: %w", vol.String(), err)
	}
	target := driver.MountTarget(filepath.Join(s.cfg.MountRoot, id))
	got, err := drv.Mount(ctx, opctx, driver.MountOpts{
		Handle:   handle,
		Node:     driver.NodeID(s.cfg.NodeID),
		Device:   dev,
		Target:   target,
		ReadOnly: false, // per-mount RO is enforced by the runner; the volume itself is RW.
	})
	if err != nil {
		// Best-effort detach so we don't leave a half-attached
		// device behind for the next reconcile.
		if derr := drv.Detach(ctx, opctx, handle, driver.NodeID(s.cfg.NodeID)); derr != nil {
			s.log.Warn("Detach after failed Mount also failed",
				log.Str("volume_id", id),
				log.Err(derr))
		}
		return fmt.Errorf("agent.volumes: mount %s: %w", vol.String(), err)
	}
	s.stateMu.Lock()
	s.mounts[id] = trackedMount{
		Driver:     drv,
		Handle:     handle,
		Target:     got,
		OpCtx:      opctx,
		VolumeNS:   vol.Namespace,
		VolumeName: vol.Name,
	}
	s.stateMu.Unlock()
	s.log.Info("Volume mounted",
		log.Str("volume_id", id),
		log.Str("namespace", vol.Namespace),
		log.Str("name", vol.Name),
		log.Str("driver", drv.Name()),
		log.Str("target", string(got)))
	return nil
}

// tearDown is the inverse: Unmount then Detach. Both are required to
// be idempotent by the Driver contract so partial failures on one side
// still let the other side clean up.
//
// The OpContext stashed on trackedMount at bringUp time is reused here:
// drivers like do-volume need their per-class config (auth refs, region)
// for Detach as much as for Attach, and the StorageClass may have been
// deleted between mount and teardown.
func (s *Subsystem) tearDown(ctx context.Context, id string, m trackedMount) error {
	var firstErr error
	if err := m.Driver.Unmount(ctx, m.OpCtx, m.Target); err != nil {
		firstErr = fmt.Errorf("agent.volumes: unmount %s: %w", id, err)
		s.log.Warn("Unmount failed; will still attempt Detach",
			log.Str("volume_id", id),
			log.Err(err))
	}
	if err := m.Driver.Detach(ctx, m.OpCtx, m.Handle, driver.NodeID(s.cfg.NodeID)); err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("agent.volumes: detach %s: %w", id, err)
		}
		return firstErr
	}
	if firstErr == nil {
		s.log.Info("Volume unmounted",
			log.Str("volume_id", id),
			log.Str("namespace", m.VolumeNS),
			log.Str("name", m.VolumeName))
	}
	return firstErr
}

// buildOpContext resolves the Volume's StorageClass + merged
// Parameters into an OpContext ready for a driver call.
//
// Resolution order matches the controller's reclaimParameters helper:
//
//  1. Live class still around → merge(class.Parameters, vol.Parameters).
//  2. Class gone but vol.DriverParameters carries a snapshot taken at
//     Provision time → use that, merged with vol.Parameters. Drivers
//     like do-volume need region / auth refs for Detach / Unmount and
//     would otherwise have nothing to work from.
//  3. Neither → volume-local Parameters only; drivers that strictly
//     require class context fail with their own error.
//
// See RUNE-200 PR 2.
func (s *Subsystem) buildOpContext(ctx context.Context, vol *types.Volume) (driver.OpContext, error) {
	opctx := driver.OpContext{
		Volume:     vol,
		Parameters: map[string]string{},
	}
	if vol.StorageClassName == "" {
		opctx.Parameters = mergeParameters(nil, vol.Parameters)
		return opctx, nil
	}
	var class types.StorageClass
	if err := s.cfg.Store.Get(ctx, types.ResourceTypeStorageClass, "", vol.StorageClassName, &class); err != nil {
		// Treat a missing class as an orphan. Prefer the snapshot when
		// the controller stamped one at Provision time; otherwise fall
		// through to volume-only parameters.
		source := "volume-only"
		if len(vol.DriverParameters) > 0 {
			source = "DriverParameters snapshot"
			opctx.Parameters = mergeParameters(vol.DriverParameters, vol.Parameters)
		} else {
			opctx.Parameters = mergeParameters(nil, vol.Parameters)
		}
		s.log.Warn("StorageClass missing during OpContext build; falling back",
			log.Str("namespace", vol.Namespace),
			log.Str("name", vol.Name),
			log.Str("storageClass", vol.StorageClassName),
			log.Str("source", source),
			log.Err(err))
		return opctx, nil
	}
	opctx.StorageClass = &class
	opctx.Parameters = mergeParameters(class.Parameters, vol.Parameters)
	return opctx, nil
}

// mergeParameters layers Volume.Parameters on top of StorageClass.Parameters.
// Mirrors the helper in the volume controller — kept package-local here
// so the agent doesn't have to import the orchestrator just for one
// map-overlay function.
func mergeParameters(class, vol map[string]string) map[string]string {
	out := make(map[string]string, len(class)+len(vol))
	for k, v := range class {
		out[k] = v
	}
	for k, v := range vol {
		out[k] = v
	}
	return out
}

// lookupTrackedID finds the in-memory tracking key for a (namespace,
// name) pair when a delete event arrives without a body. Linear scan
// is fine: the tracked map is bounded by the per-node binding count,
// which is small.
func (s *Subsystem) lookupTrackedID(namespace, name string) string {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	for id, m := range s.mounts {
		if m.VolumeNS == namespace && m.VolumeName == name {
			return id
		}
	}
	return ""
}
