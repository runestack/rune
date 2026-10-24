// Package volumes is the per-node Subsystem that drives the storage
// driver Attach/Mount/Unmount/Detach lifecycle for Volumes whose
// BoundNode equals this node's identity.
//
// It is the agent-side counterpart to pkg/orchestrator/volume:
// the controller decides where a volume is bound
// (and is the only writer of Volume.BoundNode); this subsystem watches
// those bindings and turns them into real node-local mounts under
// `<MountRoot>/<namespace>/<volume.ID>/` (default `/var/lib/rune/mounts`).
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
//   - Public accessor MountTargetFor(Volume.MountKey()) lets the runner-side
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/storage/driverparams"
	"github.com/runestack/rune/pkg/storage/mountinfo"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// DefaultMountRoot is the per-node directory under which the subsystem
// nests one <namespace>/<Volume.ID> subdirectory per volume.
const DefaultMountRoot = "/var/lib/rune/mounts"

// DefaultRetryInterval is the period at which the subsystem re-walks
// every Volume and re-attempts bringUp for any that should be mounted
// but aren't. Picked so a stuck mount recovers within one operator
// attention span without hammering cloud-provider APIs.
const DefaultRetryInterval = 30 * time.Second

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

	// NodeHostname is the OS hostname of the node (os.Hostname()).
	// Threaded into every driver.OpContext so cloud-backed drivers can
	// map the Rune node onto the cloud provider's instance identity —
	// e.g. dovolume looks up the DO droplet by hostname-derived name.
	// Empty disables that mapping; the affected driver surfaces its
	// own error.
	NodeHostname string

	// Lookup resolves a Volume to a concrete Driver instance.
	// Required.
	Lookup DriverLookup

	// SecretLookup resolves `secret:...` references inside the
	// merged driver parameters before Attach / Mount / Unmount /
	// Detach calls. nil disables resolution; secret-ref-shaped
	// values then fail the operation with a clear error. See
	// RUNE-200 PR 3.
	SecretLookup driverparams.SecretLookup

	// MountRoot is the per-node directory under which mount targets
	// live. Defaults to DefaultMountRoot.
	MountRoot string

	// RetryInterval is how often the run loop re-walks every Volume
	// and re-attempts bringUp for any that should be mounted but
	// aren't. Required so transient Attach / Mount failures recover
	// without a runed restart. Defaults to DefaultRetryInterval.
	RetryInterval time.Duration

	// MountTable reports which devices are mounted at a path. Defaults
	// to the host's /proc/self/mountinfo.
	MountTable MountTable

	// Logger; defaults to the global logger with component
	// "agent.volumes".
	Logger log.Logger
}

// MountTable is the read side of the host mount table.
type MountTable interface {
	// Stack returns the devices mounted exactly at path, bottom first.
	Stack(path string) ([]mountinfo.Dev, error)
	// MountPoints returns every path a device is mounted at.
	MountPoints(dev mountinfo.Dev) ([]string, error)
	// DeviceOf returns the device number of a block device node.
	DeviceOf(dev string) (mountinfo.Dev, error)
}

type hostMountTable struct{}

func (hostMountTable) Stack(path string) ([]mountinfo.Dev, error)    { return mountinfo.Stack(path) }
func (hostMountTable) MountPoints(d mountinfo.Dev) ([]string, error) { return mountinfo.MountPoints(d) }
func (hostMountTable) DeviceOf(dev string) (mountinfo.Dev, error)    { return mountinfo.DeviceOf(dev) }

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

	// mounts is the in-memory record of (Volume.MountKey() -> tracked
	// mount state). Protected by stateMu rather than mu so reads from
	// MountTargetFor don't contend with lifecycle calls.
	stateMu sync.RWMutex
	mounts  map[string]trackedMount

	// lastErr records why a volume is NOT mounted (Volume.MountKey() ->
	// most recent bring-up failure), so the orchestrator can tell an operator
	// the actual cause instead of a bare "not yet mounted (will retry)".
	// Cleared when the volume comes up. Guarded by stateMu.
	lastErr map[string]mountFailure

	// parked holds disks whose teardown left them attached (still
	// mounted somewhere, or the detach failed), keyed by handle. Once
	// untracked, nothing else would ever retry them, so the retry tick
	// does. Mirrored to parkedFile so a restart does not forget them.
	// Guarded by stateMu.
	parked map[driver.VolumeHandle]*parkedDisk

	// saveMu serialises writes of parkedFile: drainMounts parks from
	// several goroutines at once.
	saveMu sync.Mutex
}

// mountFailure is the most recent reason a volume failed to come up on
// this node, kept purely for operator-facing diagnostics.
type mountFailure struct {
	Err  error
	When time.Time
}

// trackedMount is the per-volume bookkeeping the subsystem keeps for
// every volume currently attached + mounted on this node.
type trackedMount struct {
	// Driver is the concrete driver that owns the handle.
	Driver driver.Driver
	// Handle is the opaque driver handle copied from Volume.Handle at
	// mount time.
	Handle driver.VolumeHandle
	// Device is the device path Attach returned, so tearDown can check
	// whether the disk is still mounted elsewhere before detaching it.
	Device driver.DevicePath
	// Target is the mount target the driver returned (drivers may
	// rewrite the proposed target).
	Target driver.MountTarget
	// OpCtx is the OpContext used to bring this mount up, retained so
	// the symmetric Unmount/Detach calls during tearDown see the same
	// per-class parameters (region, auth refs, …) the driver was given
	// at Attach/Mount time. Snapshotting here is also defence against
	// the StorageClass being deleted between bringUp and tearDown.
	//
	// Its Parameters hold `secret:...` refs already resolved to
	// plaintext, so they are exactly as old as the mount. tearDown
	// prefers a fresh resolution from RawParameters and only falls back
	// to these.
	OpCtx driver.OpContext
	// RawParameters is the same parameter map *before* secret refs were
	// resolved. Retained so tearDown can resolve credentials afresh: a
	// mount can be months old, and a rotated secret must reach Detach
	// without waiting for a remount (issue #186).
	RawParameters map[string]string
	// VolumeNS / VolumeName are kept so logs read sensibly.
	VolumeNS   string
	VolumeName string
	// VolumeID and StorageClass let a parked disk be rebuilt after a
	// restart.
	VolumeID     string
	StorageClass string
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
	// The mount table records resolved paths; compare like with like.
	if r, err := filepath.EvalSymlinks(cfg.MountRoot); err == nil {
		cfg.MountRoot = r
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = DefaultRetryInterval
	}
	if cfg.MountTable == nil {
		cfg.MountTable = hostMountTable{}
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
		lastErr: make(map[string]mountFailure),
		parked:  make(map[driver.VolumeHandle]*parkedDisk),
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

	s.loadParked()
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

	s.drainMounts(ctx, tracked)

	s.log.Info("Volume subsystem stopped")
	return nil
}

// teardownFallbackTimeout bounds a single volume's PROVIDER calls when
// the caller's context carries no deadline of its own. Provider clients
// set their own (longer) HTTP timeouts, which must not be what decides
// how long shutdown takes.
//
// It does not bound the whole teardown. The filesystem flush inside
// Driver.Unmount takes no context and is deliberately unbounded, because
// cutting it short detaches a half-written filesystem — so a shutdown can
// legitimately outlive this and any deadline the caller sets. See
// pkg/storage/driver/mountsync.
const teardownFallbackTimeout = 8 * time.Second

// drainMounts tears down every tracked mount concurrently.
//
// Concurrency is the whole fix for issue #185. Sequentially, the volumes
// shared the single agent-shutdown budget, so the first detach could
// spend all of it — on a three-volume node exactly one volume was
// released and the rest died with "context deadline exceeded". The
// volumes are independent and nothing orders them, so they go in
// parallel and stop competing for the budget.
//
// Note that there is deliberately no per-volume slice of the deadline.
// Dividing a shared budget only helps when the work is serialised; once
// it is concurrent, handing each volume anything less than the remaining
// budget would cap teardown for no benefit. teardownFallbackTimeout
// exists for the other direction — a caller with no deadline at all.
//
// A failed teardown is logged, never fatal: the volume stays attached to
// this node and the adopt-attached path re-mounts it on the next start.
func (s *Subsystem) drainMounts(ctx context.Context, tracked map[string]trackedMount) {
	if len(tracked) == 0 {
		return
	}

	// Nothing to divide up: the volumes run concurrently, so they are not
	// competing for the budget and each may simply use the caller's
	// context as-is. A caller that supplies no deadline gets one imposed,
	// so a driver's own HTTP timeout (30s for dovolume) cannot be what
	// decides how long shutdown takes.
	if err := ctx.Err(); err != nil {
		// Expired or cancelled: every driver call would fail instantly,
		// so say so once rather than once per volume.
		s.log.Warn("Shutdown budget already spent; skipping volume teardown",
			log.Int("volumes", len(tracked)),
			log.Err(err))
		return
	}
	tdCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		tdCtx, cancel = context.WithTimeout(ctx, teardownFallbackTimeout)
		defer cancel()
	}

	var wg sync.WaitGroup
	var detached atomic.Int64
	for id, m := range tracked {
		wg.Add(1)
		go func(id string, m trackedMount) {
			defer wg.Done()
			ok, err := s.tearDown(tdCtx, id, m)
			if ok {
				detached.Add(1)
			} else {
				// Recorded so the next start retries it if the volume is
				// no longer wanted on this node.
				s.park(id, m, err)
			}
			if err != nil {
				s.log.Warn("Volume teardown failed during Stop",
					log.Str("volume_id", id),
					log.Str("namespace", m.VolumeNS),
					log.Str("name", m.VolumeName),
					log.Err(err))
			}
		}(id, m)
	}
	wg.Wait()

	// One summary line, so a partial drain is obvious instead of having
	// to be inferred by counting warnings. This counts volumes actually
	// released from the node: a volume whose Unmount failed but whose
	// Detach succeeded is detached, even though tearDown returned an
	// error, and reporting it as still-attached would answer the one
	// question this line exists to answer incorrectly.
	got, total := int(detached.Load()), len(tracked)
	if got < total {
		s.log.Warn("Volume teardown incomplete",
			log.Int("detached", got),
			log.Int("total", total))
		return
	}
	s.log.Info("All volumes torn down",
		log.Int("detached", got),
		log.Int("total", total))
}

// MountTargetFor returns the host path the volume with this
// Volume.MountKey() is currently mounted at on this node (or false if the
// subsystem has not mounted it). Called by the instance controller's resolveVolumeMount via the
// MountResolver hook; the controller falls back to Volume.Handle when
// this returns false.
func (s *Subsystem) MountTargetFor(key string) (string, bool) {
	s.stateMu.RLock()
	m, ok := s.mounts[key]
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
	// Periodic retry tick. When Attach or Mount fails (e.g. transient
	// DO API hiccup, missing capability, cloud-provider rate limit),
	// the failing volume is not added to s.mounts and never receives
	// another watch event until something else updates the row.
	// Without this tick the only recovery was a runed restart; with
	// it, we re-walk every volume on RetryInterval and naturally
	// re-attempt bringUp for any that should be mounted but aren't.
	retry := time.NewTicker(s.cfg.RetryInterval)
	defer retry.Stop()
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
		case <-retry.C:
			s.retryParked(ctx)
			if err := s.initialReconcile(ctx); err != nil {
				s.log.Debug("Periodic volume re-reconcile had errors",
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
			id = vol.MountKey()
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
		return s.release(ctx, id, m)
	default:
		return nil
	}
}

// reconcile owns the per-volume decision: should this node have it
// mounted, and is the in-memory tracker in agreement?
func (s *Subsystem) reconcile(ctx context.Context, vol *types.Volume) error {
	id := vol.MountKey()

	want := s.shouldMount(vol)

	s.stateMu.RLock()
	tracked, isTracked := s.mounts[id]
	s.stateMu.RUnlock()

	switch {
	case want && !isTracked:
		return s.recordBringUp(id, s.bringUp(ctx, vol, id))
	case !want && isTracked:
		s.stateMu.Lock()
		delete(s.mounts, id)
		s.stateMu.Unlock()
		return s.release(ctx, id, tracked)
	case want && isTracked:
		// Already mounted. If the handle changed under us (the
		// controller re-provisioned), tear the old mount down and
		// re-mount.
		if tracked.Handle != driver.VolumeHandle(vol.Handle) {
			s.stateMu.Lock()
			delete(s.mounts, id)
			s.stateMu.Unlock()
			if err := s.release(ctx, id, tracked); err != nil {
				s.log.Warn("Tear-down before re-mount failed",
					log.Str("volume_id", id),
					log.Err(err))
			}
			return s.recordBringUp(id, s.bringUp(ctx, vol, id))
		}
	}
	return nil
}

// recordBringUp remembers why a volume failed to come up (or clears the
// previous reason on success) so MountErrorFor can explain a "not yet
// mounted" instance failure. Returns err unchanged for call-site brevity.
func (s *Subsystem) recordBringUp(id string, err error) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if err != nil {
		s.lastErr[id] = mountFailure{Err: err, When: time.Now()}
	} else {
		delete(s.lastErr, id)
	}
	return err
}

// MountErrorFor returns the most recent reason the named volume failed to
// mount on this node, if any. The orchestrator uses it to turn a bare
// "not yet mounted (will retry)" into an actionable message — the incident
// that motivated this had a volume attached and mounted the whole time
// while an expired provider token made the reconcile fail invisibly.
func (s *Subsystem) MountErrorFor(key string) (string, bool) {
	s.stateMu.RLock()
	f, ok := s.lastErr[key]
	s.stateMu.RUnlock()
	if !ok || f.Err == nil {
		return "", false
	}
	return f.Err.Error(), true
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
	rawctx, err := s.buildOpContextRaw(ctx, vol)
	if err != nil {
		return fmt.Errorf("agent.volumes: build OpContext for %s: %w", vol.String(), err)
	}
	// Keep the pre-resolution map so tearDown can re-resolve secrets
	// later; the driver itself only ever sees the resolved copy.
	rawParams := mergeParameters(rawctx.Parameters, nil)
	opctx, err := s.resolveSecretsOnOpContext(ctx, vol, rawctx)
	if err != nil {
		return fmt.Errorf("agent.volumes: build OpContext for %s: %w", vol.String(), err)
	}
	handle := driver.VolumeHandle(vol.Handle)
	dev, err := drv.Attach(ctx, opctx, handle, driver.NodeID(s.cfg.NodeID))
	if err != nil {
		return fmt.Errorf("agent.volumes: attach %s: %w", vol.String(), err)
	}
	target, err := s.mountTarget(vol, id, dev)
	if err != nil {
		// The disk may already be mounted, and in use, at a path from
		// before mount paths carried the namespace.
		if len(s.stillMounted(dev)) == 0 {
			if derr := drv.Detach(ctx, opctx, handle, driver.NodeID(s.cfg.NodeID)); derr != nil {
				s.log.Warn("Detach after refused mount target also failed",
					log.Str("volume_id", id),
					log.Err(derr))
			}
		}
		return err
	}
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
		if where := s.stillMounted(dev); len(where) > 0 {
			s.log.Warn("Not detaching after failed Mount: the disk is mounted elsewhere",
				log.Str("volume_id", id),
				log.Str("mounted_at", strings.Join(where, ", ")))
		} else if derr := drv.Detach(ctx, opctx, handle, driver.NodeID(s.cfg.NodeID)); derr != nil {
			s.log.Warn("Detach after failed Mount also failed",
				log.Str("volume_id", id),
				log.Err(derr))
		}
		return fmt.Errorf("agent.volumes: mount %s: %w", vol.String(), err)
	}
	s.stateMu.Lock()
	s.mounts[id] = trackedMount{
		Driver:        drv,
		Handle:        handle,
		Device:        dev,
		Target:        got,
		OpCtx:         opctx,
		RawParameters: rawParams,
		VolumeNS:      vol.Namespace,
		VolumeName:    vol.Name,
		VolumeID:      vol.ID,
		StorageClass:  vol.StorageClassName,
	}
	s.stateMu.Unlock()
	s.unpark(handle)
	s.log.Info("Volume mounted",
		log.Str("volume_id", id),
		log.Str("namespace", vol.Namespace),
		log.Str("name", vol.Name),
		log.Str("driver", drv.Name()),
		log.Str("target", string(got)))
	return nil
}

// mountTarget picks where to mount vol: <MountRoot>/<namespace>/<id>.
//
// Mount paths used to omit the namespace (<MountRoot>/<id>), so the same
// service cast into two namespaces put two disks on one path, the second
// mounted over the first. Containers already running are bound to that
// old path, so a volume whose disk is still the one visible there keeps
// it. The disk, not the namespace, decides: it is what a container bound
// to the path actually sees, so the path can never be handed to the
// wrong namespace's volume.
func (s *Subsystem) mountTarget(vol *types.Volume, key string, dev driver.DevicePath) (driver.MountTarget, error) {
	if err := pathSafeKey(key); err != nil {
		return "", fmt.Errorf("agent.volumes: %s: %w", vol.String(), err)
	}
	target := filepath.Join(s.cfg.MountRoot, key)
	// Already on the new path (runed restarted without draining): stay
	// there, even if the old path has since become free. Another disk
	// there is refused: a driver's Mount sees "something is mounted" and
	// would report success with the wrong disk.
	if dev != "" {
		if own, err := s.cfg.MountTable.DeviceOf(string(dev)); err == nil {
			if stack, err := s.cfg.MountTable.Stack(target); err == nil && len(stack) > 0 {
				if stack[len(stack)-1] == own {
					return driver.MountTarget(target), nil
				}
				return "", fmt.Errorf("agent.volumes: %s: another disk (%s) is mounted at %s", vol.String(), stack[len(stack)-1], target)
			}
		}
	}
	if legacy, ok := s.legacyTarget(vol, key, dev); ok {
		return driver.MountTarget(legacy), nil
	}
	// A volume mounted under the old layout whose ID equals this
	// namespace's name sits exactly where the namespace directory goes.
	// Creating the target there would write into that volume's disk.
	nsDir := filepath.Dir(target)
	if stack, err := s.cfg.MountTable.Stack(nsDir); err == nil && len(stack) > 0 {
		return "", fmt.Errorf("agent.volumes: %s: %s is a mount point, so the mount path %s would land inside another volume; "+
			"it clears when that volume is unmounted", vol.String(), nsDir, target)
	}
	return driver.MountTarget(target), nil
}

// pathSafeKey checks a MountKey is exactly "<namespace>/<id>" with no
// segment that could walk out of the mount root. IDs are validated where
// they are created; this holds for rows that predate that.
func pathSafeKey(key string) error {
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		return fmt.Errorf("unsafe mount key %q", key)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "\\\x00") {
			return fmt.Errorf("unsafe mount key %q", key)
		}
	}
	return nil
}

// legacyTarget reports whether vol's disk is the visible mount at its
// pre-namespace path, and logs when that path has more than one disk
// stacked on it.
func (s *Subsystem) legacyTarget(vol *types.Volume, key string, dev driver.DevicePath) (string, bool) {
	if vol.ID == "" || dev == "" {
		return "", false
	}
	legacy := filepath.Join(s.cfg.MountRoot, vol.ID)
	stack, err := s.cfg.MountTable.Stack(legacy)
	if err != nil || len(stack) == 0 {
		return "", false
	}
	own, err := s.cfg.MountTable.DeviceOf(string(dev))
	if err != nil {
		s.log.Warn("Cannot identify the volume's device; not reusing the old mount path",
			log.Str("volume_id", key), log.Str("device", string(dev)), log.Err(err))
		return "", false
	}
	pos := -1
	for i, d := range stack {
		if d == own {
			pos = i
		}
	}
	top := len(stack) - 1
	switch {
	case pos == top && top == 0:
		return legacy, true
	case pos == top:
		s.log.Error("Several disks are mounted on one volume path; this volume's is on top. "+
			"Containers started before it was mounted there may be using a disk from another namespace: replace them with rune restart",
			log.Str("volume_id", key), log.Str("path", legacy), log.Int("disks", len(stack)))
		return legacy, true
	case pos >= 0:
		s.log.Error("This volume's disk is mounted at its old path, but another disk was mounted over it. "+
			"Containers started since then that use this path see the other disk: replace them with rune restart. "+
			"When this volume is torn down its disk stays attached until the disk above it is unmounted",
			log.Str("volume_id", key), log.Str("path", legacy), log.Int("disks", len(stack)))
	}
	return "", false
}

// ownMounts returns the paths under MountRoot where the disk is mounted,
// split by whether it is the visible (top) mount there.
func (s *Subsystem) ownMounts(dev driver.DevicePath) (top, buried []string) {
	if dev == "" {
		return nil, nil
	}
	d, err := s.cfg.MountTable.DeviceOf(string(dev))
	if err != nil {
		return nil, nil
	}
	where, err := s.cfg.MountTable.MountPoints(d)
	if err != nil {
		return nil, nil
	}
	root := s.cfg.MountRoot + string(filepath.Separator)
	for _, p := range where {
		if !strings.HasPrefix(p, root) {
			continue
		}
		stack, err := s.cfg.MountTable.Stack(p)
		if err != nil || len(stack) == 0 {
			continue
		}
		if stack[len(stack)-1] == d {
			top = append(top, p)
		} else {
			buried = append(buried, p)
		}
	}
	return top, buried
}

func (s *Subsystem) buried(dev driver.DevicePath) bool {
	_, b := s.ownMounts(dev)
	return len(b) > 0
}

// unmountLeftovers unmounts the volume's own disk wherever else it is the
// visible mount under MountRoot: its pre-namespace path, once a disk
// stacked over it has gone. The volume is being torn down, so those
// mounts go with it.
func (s *Subsystem) unmountLeftovers(ctx context.Context, id string, m trackedMount, opctx driver.OpContext) {
	top, _ := s.ownMounts(m.Device)
	for _, p := range top {
		if p == string(m.Target) {
			continue
		}
		if err := m.Driver.Unmount(ctx, opctx, driver.MountTarget(p)); err != nil {
			s.log.Warn("Unmounting the volume's leftover mount failed",
				log.Str("volume_id", id), log.Str("path", p), log.Err(err))
		}
	}
}

// stillMounted returns the paths dev is still mounted at on this host.
// A disk can be mounted at two paths: one buried under another
// namespace's disk at its old path, and one at its namespaced path.
// Unmounting the second must not detach a disk the first still holds.
// It answers only on positive evidence; if the device or the mount
// table cannot be read it returns nothing and the detach goes ahead.
func (s *Subsystem) stillMounted(dev driver.DevicePath) []string {
	if dev == "" {
		return nil
	}
	d, err := s.cfg.MountTable.DeviceOf(string(dev))
	if err != nil {
		s.log.Warn("Cannot tell whether the disk is still mounted; detaching anyway",
			log.Str("device", string(dev)), log.Err(err))
		return nil
	}
	where, err := s.cfg.MountTable.MountPoints(d)
	if err != nil {
		s.log.Warn("Cannot read the mount table; detaching anyway",
			log.Str("device", string(dev)), log.Err(err))
		return nil
	}
	return where
}

// unmountBlocked says why unmounting path could take down another disk's
// mount, or "" when it is safe: another disk is the visible mount there,
// or this disk's device cannot be identified while something is mounted
// there. unknown reports the second case, where the disk's own state is
// unknown too, so it must not be detached either. A volume with no
// recorded device (drivers that mount no block device) is never blocked.
func (s *Subsystem) unmountBlocked(dev driver.DevicePath, path string) (reason string, unknown bool) {
	if dev == "" {
		return "", false
	}
	own, err := s.cfg.MountTable.DeviceOf(string(dev))
	if err != nil {
		stack, serr := s.cfg.MountTable.Stack(path)
		switch {
		case errors.Is(serr, mountinfo.ErrUnsupported):
			return "", false // no host mount table to consult on this platform
		case serr != nil:
			return fmt.Sprintf("cannot identify this volume's disk (%v) or read the mount table (%v)", err, serr), true
		case len(stack) > 0:
			return fmt.Sprintf("cannot identify this volume's disk (%v) and something is mounted at its path", err), true
		}
		return "", false
	}
	stack, err := s.cfg.MountTable.Stack(path)
	if err != nil || len(stack) == 0 {
		return "", false
	}
	if stack[len(stack)-1] != own {
		return "another disk is now mounted at this volume's path", false
	}
	return "", false
}

// tearDown is the inverse: Unmount then Detach. Both are required to
// be idempotent by the Driver contract so partial failures on one side
// still let the other side clean up.
//
// The per-class config a driver needs for Detach (region, auth refs)
// comes from teardownOpContext, which starts from the OpContext stashed
// at bringUp — the StorageClass may have been deleted since — but
// re-resolves its secret refs so a rotated credential is used rather
// than the one frozen at mount time (issue #186).
//
// detached reports whether the volume is off this node, which is not the
// same as err == nil: Unmount can fail while Detach succeeds. Callers
// that report "is this volume still attached" must use detached, not the
// error.
func (s *Subsystem) tearDown(ctx context.Context, id string, m trackedMount) (detached bool, err error) {
	opctx := s.teardownOpContext(ctx, id, m)
	// Named before the call, not after: Unmount flushes the filesystem
	// first, which on a volume with a lot of dirty data is the longest
	// unattended pause in a shutdown. Without this line the operator sees
	// systemd hang with no indication of which volume, or whether it is
	// working at all.
	s.log.Info("Unmounting volume",
		log.Str("volume_id", id),
		log.Str("namespace", m.VolumeNS),
		log.Str("name", m.VolumeName),
		log.Str("target", string(m.Target)))
	var firstErr error
	// The path may since have been given to another disk (the handle
	// changed and the new disk mounted there): leave that mount alone.
	reason, unknown := s.unmountBlocked(m.Device, string(m.Target))
	if reason != "" {
		s.log.Warn("Not unmounting volume path",
			log.Str("volume_id", id), log.Str("target", string(m.Target)), log.Str("reason", reason))
		if unknown {
			// This disk may be the one still mounted there: detaching
			// it would pull it from under that mount. Park and retry.
			return false, fmt.Errorf("agent.volumes: not detaching %s: %s", id, reason)
		}
	} else if uerr := m.Driver.Unmount(ctx, opctx, m.Target); uerr != nil {
		firstErr = fmt.Errorf("agent.volumes: unmount %s: %w", id, uerr)
		s.log.Warn("Unmount failed",
			log.Str("volume_id", id),
			log.Err(uerr))
	}
	s.unmountLeftovers(ctx, id, m, opctx)
	if where := s.stillMounted(m.Device); len(where) > 0 {
		err := fmt.Errorf("agent.volumes: not detaching %s: its disk is still mounted at %s", id, strings.Join(where, ", "))
		s.log.Error("Not detaching volume: its disk is still mounted",
			log.Str("volume_id", id),
			log.Str("mounted_at", strings.Join(where, ", ")))
		if firstErr == nil {
			firstErr = err
		}
		return false, firstErr
	}
	if derr := m.Driver.Detach(ctx, opctx, m.Handle, driver.NodeID(s.cfg.NodeID)); derr != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("agent.volumes: detach %s: %w", id, derr)
		}
		return false, firstErr
	}
	if firstErr == nil {
		s.log.Info("Volume unmounted",
			log.Str("volume_id", id),
			log.Str("namespace", m.VolumeNS),
			log.Str("name", m.VolumeName))
	}
	return true, firstErr
}

// teardownOpContext returns the OpContext to use for Unmount/Detach.
//
// It prefers credentials resolved *now* over the ones captured when the
// mount was brought up. Detach mostly runs during shutdown, so the
// bring-up copy is as old as the mount: after rotating a provider
// token, the very next teardown would still present the pre-rotation
// value and fail with the provider's auth error — which reads exactly
// like "the new token is wrong", and cannot be fixed by rotating again
// (issue #186).
//
// Resolution failures are deliberately non-fatal. Teardown runs while
// the process is going away and the secret lookup may itself be a
// casualty of that; falling back to the captured OpContext keeps the
// previous behaviour rather than turning a stale credential into no
// credential at all.
func (s *Subsystem) teardownOpContext(ctx context.Context, id string, m trackedMount) driver.OpContext {
	if len(m.RawParameters) == 0 || s.cfg.SecretLookup == nil {
		return m.OpCtx // pre-#186 mount, or nothing to resolve against
	}
	ns := m.VolumeNS
	if m.OpCtx.Volume != nil && m.OpCtx.Volume.Namespace != "" {
		ns = m.OpCtx.Volume.Namespace
	}
	resolved, err := driverparams.Resolve(ctx, m.RawParameters, ns, s.cfg.SecretLookup)
	if err != nil {
		s.log.Warn("Could not re-resolve credentials for teardown; using the values from mount time",
			log.Str("volume_id", id),
			log.Str("namespace", m.VolumeNS),
			log.Str("name", m.VolumeName),
			log.Err(err))
		return m.OpCtx
	}
	opctx := m.OpCtx
	opctx.Parameters = resolved
	return opctx
}

// buildOpContextRaw resolves the Volume's StorageClass + merged
// Parameters into an OpContext ready for a driver call, leaving any
// `secret:...` refs intact.
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
// Callers pass the result through resolveSecretsOnOpContext before
// handing it to a driver, so the driver never sees a secret reference,
// only literal values. Splitting the two steps lets bringUp keep the
// unresolved map on the trackedMount, so tearDown can resolve a rotated
// credential afresh instead of replaying the one captured at mount time
// (issue #186). See RUNE-200 PR 2 + PR 3.
func (s *Subsystem) buildOpContextRaw(ctx context.Context, vol *types.Volume) (driver.OpContext, error) {
	opctx := driver.OpContext{
		Volume:       vol,
		Parameters:   map[string]string{},
		NodeHostname: s.cfg.NodeHostname,
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

// resolveSecretsOnOpContext walks opctx.Parameters and replaces any
// `secret:...` ref with the resolved secret value via cfg.SecretLookup.
// Returns the opctx unchanged on success or a wrapped error naming the
// offending volume so the agent log identifies the row.
func (s *Subsystem) resolveSecretsOnOpContext(ctx context.Context, vol *types.Volume, opctx driver.OpContext) (driver.OpContext, error) {
	resolved, err := driverparams.Resolve(ctx, opctx.Parameters, vol.Namespace, s.cfg.SecretLookup)
	if err != nil {
		return driver.OpContext{}, fmt.Errorf("agent.volumes: resolve secret refs for %s/%s: %w", vol.Namespace, vol.Name, err)
	}
	opctx.Parameters = resolved
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
