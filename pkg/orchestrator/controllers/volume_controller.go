package controllers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// VolumeController owns the lifecycle of Volume resources: it watches the
// store, picks a driver from the resolved StorageClass, and drives the
// Pending → Provisioning → Available state machine. On deletion it honours
// the reclaim policy (delete via the driver, or leave the handle in place
// for retain).
//
// Scope for this iteration is intentionally narrow:
//
//   - No claim binding (that lives with the runner-integration ticket;
//     this controller leaves Volume.BoundClaim untouched).
//
//   - No expand reconciliation (the Volume size field is observed but not
//     compared to the existing handle).
//
//   - No snapshot scheduling (handled in RUNE-071).
//
// Introduced in RUNE-069.
type VolumeController interface {
	Start(ctx context.Context) error
	Stop() error
}

// VolumeControllerOptions configures the controller. DriverConfigs is the
// per-driver-name configuration block parsed out of the runefile [storage]
// section — for example `{"local": {"localVolumeRoot": "/var/lib/rune"}}`.
// nil/missing entries are passed to the factory as nil, which all
// in-tree drivers tolerate.
type VolumeControllerOptions struct {
	Store         store.Store
	Logger        log.Logger
	DriverConfigs map[string]map[string]any

	// DefaultStorageClass mirrors the runefile [storage].defaultStorageClass
	// knob. nil keeps the built-in default ("local"); a non-nil pointer
	// to a non-empty name promotes that class to Default:true at boot
	// (creating it as a no-op marker if needed); a non-nil pointer to
	// the empty string disables the cluster default entirely (any
	// existing Default flag is cleared, and Volumes with empty
	// storageClassName fail to resolve).
	DefaultStorageClass *string

	// PreserveOnDelete, when true, demotes ReclaimPolicy:delete to
	// retain for volumes provisioned by the in-tree "local" driver. Has
	// no effect on other drivers.
	PreserveOnDelete bool

	// Provision retry tuning. Zero values fall back to defaults
	// (5 attempts, 2s base, 30s cap) so existing callers don't need
	// updating.
	MaxProvisionAttempts int
	ProvisionBaseBackoff time.Duration
	ProvisionMaxBackoff  time.Duration
}

// volumeController is the default VolumeController.
//
// In addition to the Volume reconciliation loop, the controller also owns
// the at-most-one-Default `StorageClass` invariant. The full design (see
// RUNE-073) calls for the API server to enforce this on the write path,
// but until that lands the controller re-asserts the invariant at boot
// (after seeding) and on every StorageClass Create/Update event: when a
// new class arrives with `Default:true`, all other Default classes are
// flipped to `false` so the most recent write wins.
type volumeController struct {
	store         store.Store
	logger        log.Logger
	driverConfigs map[string]map[string]any

	// Operator-supplied [storage] knobs.
	defaultStorageClass *string
	preserveOnDelete    bool

	// Cached driver instances keyed by driver name. Drivers are stateless
	// for the purposes of the controller, so a single instance per
	// configured driver name is enough.
	driverMu sync.RWMutex
	drivers  map[string]driver.Driver

	// Bookkeeping for self-triggered updates so the watch loop doesn't
	// reconcile its own status writes into oblivion.
	internalUpdates sync.Map

	// Same idea for StorageClass writes from the uniqueness enforcer.
	internalSCUpdates sync.Map

	// Provision retry/backoff state. Indexed by "<namespace>/<name>".
	// retries holds the attempt count so far; retryTimers holds the
	// pending re-arm timer (if any). Both are protected by retryMu.
	retryMu     sync.Mutex
	retries     map[string]int
	retryTimers map[string]*time.Timer
	maxAttempts int
	baseBackoff time.Duration
	maxBackoff  time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewVolumeController constructs a VolumeController.
func NewVolumeController(opts VolumeControllerOptions) (VolumeController, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("volume controller: store is required")
	}
	if opts.Logger == nil {
		return nil, fmt.Errorf("volume controller: logger is required")
	}
	maxAttempts := opts.MaxProvisionAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	base := opts.ProvisionBaseBackoff
	if base <= 0 {
		base = 2 * time.Second
	}
	max := opts.ProvisionMaxBackoff
	if max <= 0 {
		max = 30 * time.Second
	}
	if max < base {
		max = base
	}
	return &volumeController{
		store:               opts.Store,
		logger:              opts.Logger.WithComponent("volume-controller"),
		driverConfigs:       opts.DriverConfigs,
		defaultStorageClass: opts.DefaultStorageClass,
		preserveOnDelete:    opts.PreserveOnDelete,
		drivers:             make(map[string]driver.Driver),
		retries:             make(map[string]int),
		retryTimers:         make(map[string]*time.Timer),
		maxAttempts:         maxAttempts,
		baseBackoff:         base,
		maxBackoff:          max,
	}, nil
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func (c *volumeController) Start(ctx context.Context) error {
	c.ctx, c.cancel = context.WithCancel(ctx)
	if err := c.seedBuiltInStorageClasses(c.ctx); err != nil {
		// Seeding failure must not prevent the controller from starting:
		// the watch loop is the source of truth and the operator can recover
		// by hand-creating the classes. Log loudly and continue.
		c.logger.Error("Failed to seed built-in storage classes", log.Err(err))
	}
	if err := c.applyDefaultStorageClassConfig(c.ctx); err != nil {
		c.logger.Error("Failed to apply [storage].defaultStorageClass", log.Err(err))
	}
	keep := ""
	if c.defaultStorageClass != nil {
		keep = *c.defaultStorageClass
	}
	if err := c.enforceDefaultStorageClassUniqueness(c.ctx, keep); err != nil {
		c.logger.Error("Failed to enforce Default StorageClass uniqueness at boot", log.Err(err))
	}
	watchCh, err := c.store.Watch(c.ctx, types.ResourceTypeVolume, "")
	if err != nil {
		return fmt.Errorf("volume controller: watch: %w", err)
	}
	scWatchCh, err := c.store.Watch(c.ctx, types.ResourceTypeStorageClass, "")
	if err != nil {
		return fmt.Errorf("volume controller: storage-class watch: %w", err)
	}
	c.wg.Add(2)
	go c.run(watchCh)
	go c.runStorageClassWatch(scWatchCh)
	c.logger.Info("Volume controller started")
	return nil
}

func (c *volumeController) Stop() error {
	if c.cancel != nil {
		c.cancel()
	}
	c.retryMu.Lock()
	for k, t := range c.retryTimers {
		t.Stop()
		delete(c.retryTimers, k)
	}
	c.retryMu.Unlock()
	c.wg.Wait()
	c.logger.Info("Volume controller stopped")
	return nil
}

func (c *volumeController) run(initial <-chan store.WatchEvent) {
	defer c.wg.Done()
	watchCh := initial
	for {
		select {
		case <-c.ctx.Done():
			return
		case ev, ok := <-watchCh:
			if !ok {
				// Re-establish watch on close — the badger store recreates
				// channels across restarts.
				next, err := c.store.Watch(c.ctx, types.ResourceTypeVolume, "")
				if err != nil {
					if errors.Is(c.ctx.Err(), context.Canceled) {
						return
					}
					c.logger.Error("Volume watch restart failed",
						log.Err(err))
					time.Sleep(2 * time.Second)
					continue
				}
				watchCh = next
				continue
			}
			if c.isSelfUpdate(ev) {
				continue
			}
			if err := c.handle(c.ctx, ev); err != nil {
				c.logger.Error("Volume event handling failed",
					log.Str("namespace", ev.Namespace),
					log.Str("name", ev.Name),
					log.Str("type", string(ev.Type)),
					log.Err(err))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Event dispatch
// ---------------------------------------------------------------------------

func (c *volumeController) handle(ctx context.Context, ev store.WatchEvent) error {
	switch ev.Type {
	case store.WatchEventCreated, store.WatchEventUpdated:
		vol, ok := ev.Resource.(*types.Volume)
		if !ok {
			return fmt.Errorf("expected *types.Volume, got %T", ev.Resource)
		}
		return c.reconcile(ctx, vol)
	case store.WatchEventDeleted:
		vol, ok := ev.Resource.(*types.Volume)
		if !ok {
			// Some store backends omit the body on delete events.
			c.logger.Warn("Volume delete event missing body",
				log.Str("namespace", ev.Namespace),
				log.Str("name", ev.Name))
			return nil
		}
		return c.handleDeleted(ctx, vol)
	default:
		return nil
	}
}

// reconcile drives Pending/Provisioning volumes to Available. It is a no-op
// for volumes already in a terminal-ish state (Available, Bound, Released,
// Failed, Stalled) — those transitions belong to the binder/claim system,
// not here.
//
// Pending entry implicitly clears any in-memory retry/backoff bookkeeping
// (see clearRetry) so an explicit operator retry — `rune volume
// retry-provision` flips status back to Pending via the API — restarts
// the attempt counter from 1. Provisioning re-entry preserves it so a
// scheduled retry continues to count up against MaxProvisionAttempts.
func (c *volumeController) reconcile(ctx context.Context, vol *types.Volume) error {
	switch vol.Status {
	case "", types.VolumeStatusPending:
		// Fresh attempt — reset retry state.
		c.clearRetry(volumeKey(vol.Namespace, vol.Name))
	case types.VolumeStatusProvisioning:
		// Mid-flight; preserve retry counter.
	default:
		return nil
	}

	class, err := c.resolveStorageClass(ctx, vol)
	if err != nil {
		return c.markFailed(ctx, vol, "StorageClassMissing", err.Error())
	}

	d, err := c.driverFor(class.Driver)
	if err != nil {
		return c.markFailed(ctx, vol, "DriverUnavailable", err.Error())
	}

	merged := mergeParameters(class.Parameters, vol.Parameters)

	// Mark Provisioning before the call so an operator watching the
	// resource sees the transition even on a long-running provision.
	if vol.Status != types.VolumeStatusProvisioning {
		if err := c.updateStatus(ctx, vol, types.VolumeStatusProvisioning, "", ""); err != nil {
			return err
		}
	}

	// Restore-from-snapshot path: when a Volume carries the
	// well-known parameter "rune.io/restoreFromSnapshot=<ns>/<name>",
	// route this reconcile through Driver.RestoreFromSnapshot instead
	// of Driver.Provision. The SnapshotService stamps the parameter
	// when handling RestoreVolume.
	if ref, ok := merged[types.RestoreFromSnapshotParam]; ok && ref != "" {
		handle, err := c.restoreFromSnapshot(ctx, vol, class, merged, ref, d)
		if err != nil {
			return c.handleProvisionFail(ctx, vol, err)
		}
		vol.Handle = string(handle)
		c.clearRetry(volumeKey(vol.Namespace, vol.Name))
		return c.updateStatus(ctx, vol, types.VolumeStatusAvailable, "", "")
	}

	sizeBytes, err := volumeSizeBytes(vol)
	if err != nil {
		// An unparseable Size is a spec error, not a transient driver
		// failure — fail terminally rather than burn retry budget.
		return c.markFailed(ctx, vol, "InvalidSize", err.Error())
	}

	opctx := driver.OpContext{
		StorageClass: class,
		Volume:       vol,
		Parameters:   merged,
	}
	handle, err := d.Provision(ctx, opctx, driver.ProvisionRequest{
		SizeBytes: sizeBytes,
	})
	if err != nil {
		return c.handleProvisionFail(ctx, vol, err)
	}

	vol.Handle = string(handle)
	// Snapshot the merged driver parameters onto the Volume so that
	// reclaim Delete / agent Detach / agent Unmount have something to
	// consult even if the StorageClass is deleted before its volumes.
	// Stores a copy so subsequent mutations to `merged` don't leak.
	vol.DriverParameters = copyStringMap(merged)
	c.clearRetry(volumeKey(vol.Namespace, vol.Name))
	return c.updateStatus(ctx, vol, types.VolumeStatusAvailable, "", "")
}

// handleProvisionFail records a Provision attempt failure, schedules a
// retry with exponential backoff, and transitions the volume to
// VolumeStatusStalled once MaxProvisionAttempts has been exceeded.
//
// While retries are scheduled the volume sits at VolumeStatusFailed with
// reason "ProvisionFailedWillRetry" and a message that includes the
// remaining attempt count + next-retry delay; once exhausted, it
// transitions to VolumeStatusStalled with reason
// "ProvisionRetriesExhausted" — terminal until an operator runs
// `rune volume retry-provision`.
func (c *volumeController) handleProvisionFail(ctx context.Context, vol *types.Volume, provErr error) error {
	key := volumeKey(vol.Namespace, vol.Name)

	c.retryMu.Lock()
	if t := c.retryTimers[key]; t != nil {
		t.Stop()
		delete(c.retryTimers, key)
	}
	attempts := c.retries[key] + 1
	c.retries[key] = attempts

	if attempts >= c.maxAttempts {
		delete(c.retries, key)
		c.retryMu.Unlock()
		c.logger.Warn("Volume provision retries exhausted; marking Stalled",
			log.Str("namespace", vol.Namespace),
			log.Str("name", vol.Name),
			log.Int("attempts", attempts),
			log.Err(provErr))
		return c.updateStatus(ctx, vol, types.VolumeStatusStalled,
			"ProvisionRetriesExhausted",
			fmt.Sprintf("provision failed after %d attempts: %v (rune volume retry-provision to re-arm)", attempts, provErr))
	}

	delay := c.backoffFor(attempts)
	ns, name := vol.Namespace, vol.Name
	c.retryTimers[key] = time.AfterFunc(delay, func() {
		c.retryProvision(ns, name)
	})
	c.retryMu.Unlock()

	c.logger.Info("Volume provision failed; scheduling retry",
		log.Str("namespace", vol.Namespace),
		log.Str("name", vol.Name),
		log.Int("attempt", attempts),
		log.Int("max", c.maxAttempts),
		log.Duration("delay", delay),
		log.Err(provErr))
	return c.updateStatus(ctx, vol, types.VolumeStatusFailed,
		"ProvisionFailedWillRetry",
		fmt.Sprintf("attempt %d/%d failed: %v (next retry in %s)", attempts, c.maxAttempts, provErr, delay))
}

// retryProvision is invoked by the per-volume backoff timer. It re-fetches
// the current Volume row and, if it's still in a retryable state, drives
// a fresh Provision attempt without resetting the retry counter (the
// timer-driven path uses VolumeStatusProvisioning so reconcile preserves
// the count).
func (c *volumeController) retryProvision(ns, name string) {
	if c.ctx == nil || c.ctx.Err() != nil {
		return
	}
	c.retryMu.Lock()
	delete(c.retryTimers, volumeKey(ns, name))
	c.retryMu.Unlock()

	var v types.Volume
	if err := c.store.Get(c.ctx, types.ResourceTypeVolume, ns, name, &v); err != nil {
		c.logger.Warn("Volume retry: get failed",
			log.Str("namespace", ns), log.Str("name", name), log.Err(err))
		return
	}
	// If the operator has moved the volume out of the retry path (e.g.
	// deleted it, or RetryProvision flipped it to Pending which the
	// watch loop is already reconciling), bail.
	if v.Status != types.VolumeStatusFailed {
		return
	}
	// Force into Provisioning locally so reconcile preserves the
	// in-memory attempt counter.
	v.Status = types.VolumeStatusProvisioning
	if err := c.reconcile(c.ctx, &v); err != nil {
		c.logger.Error("Volume retry: reconcile failed",
			log.Str("namespace", ns), log.Str("name", name), log.Err(err))
	}
}

// backoffFor returns the delay to wait before the (attempt+1)-th retry.
// Exponential with a cap at maxBackoff; attempt is 1-based.
func (c *volumeController) backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := c.baseBackoff
	for i := 1; i < attempt && d < c.maxBackoff; i++ {
		d *= 2
	}
	if d > c.maxBackoff {
		d = c.maxBackoff
	}
	return d
}

// clearRetry stops any pending backoff timer and forgets the attempt
// counter for the given volume key. Safe to call when no entry exists.
func (c *volumeController) clearRetry(key string) {
	c.retryMu.Lock()
	defer c.retryMu.Unlock()
	if t := c.retryTimers[key]; t != nil {
		t.Stop()
		delete(c.retryTimers, key)
	}
	delete(c.retries, key)
}

// handleDeleted runs the reclaim path. The store has already removed the
// Volume row; we only need to honour the policy that was on it.
func (c *volumeController) handleDeleted(ctx context.Context, vol *types.Volume) error {
	if vol.Handle == "" {
		// Never provisioned — nothing to clean up.
		return nil
	}
	switch vol.ReclaimPolicy {
	case types.ReclaimPolicyRetain, "":
		c.logger.Info("Retaining volume handle on delete",
			log.Str("namespace", vol.Namespace),
			log.Str("name", vol.Name),
			log.Str("handle", vol.Handle))
		return nil
	case types.ReclaimPolicyDelete:
		// fall through
	default:
		c.logger.Warn("Unknown reclaim policy; defaulting to retain",
			log.Str("policy", string(vol.ReclaimPolicy)),
			log.Str("namespace", vol.Namespace),
			log.Str("name", vol.Name))
		return nil
	}

	// Resolve driver: prefer the StorageClass on the dying volume, but
	// it may already be gone if the operator deleted both at once. Fall
	// back to the volume's parameters / cached driver.
	driverName := ""
	var class *types.StorageClass
	if c, err := c.resolveStorageClass(ctx, vol); err == nil {
		class = c
		driverName = c.Driver
	}
	if driverName == "" {
		c.logger.Warn("Cannot resolve driver for reclaim; leaving handle in place",
			log.Str("namespace", vol.Namespace),
			log.Str("name", vol.Name),
			log.Str("handle", vol.Handle))
		return nil
	}
	// Honour the runefile [storage].preserveOnDelete knob: when set and
	// the volume was provisioned by the in-tree "local" driver, treat
	// reclaimPolicy:delete as retain. Other drivers (operator-owned
	// host paths, cloud block devices) are unaffected.
	if c.preserveOnDelete && driverName == "local" {
		c.logger.Info("Preserving local volume on delete (storage.preserveOnDelete=true)",
			log.Str("namespace", vol.Namespace),
			log.Str("name", vol.Name),
			log.Str("handle", vol.Handle))
		return nil
	}
	d, err := c.driverFor(driverName)
	if err != nil {
		return fmt.Errorf("reclaim: driver %q: %w", driverName, err)
	}
	// Build the OpContext for Delete. When the class is gone (orphan
	// reclaim), StorageClass is nil and Parameters is the volume-local
	// snapshot only. PR 2 will fold in Volume.Metadata.DriverParameters
	// for the orphan case.
	opctx := driver.OpContext{
		StorageClass: class,
		Volume:       vol,
		Parameters:   reclaimParameters(class, vol),
	}
	if err := d.Delete(ctx, opctx, driver.VolumeHandle(vol.Handle)); err != nil {
		return fmt.Errorf("reclaim: driver Delete: %w", err)
	}
	c.logger.Info("Reclaimed volume",
		log.Str("namespace", vol.Namespace),
		log.Str("name", vol.Name),
		log.Str("handle", vol.Handle),
		log.Str("driver", driverName))
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (c *volumeController) resolveStorageClass(ctx context.Context, vol *types.Volume) (*types.StorageClass, error) {
	name := vol.StorageClassName
	if name == "" {
		// Fall back to the cluster-default class. Both the API server
		// (RUNE-073) and the seeder maintain the at-most-one invariant; if
		// somehow zero or many are flagged here we treat it as a hard error
		// rather than guessing.
		def, err := c.findDefaultStorageClass(ctx)
		if err != nil {
			return nil, fmt.Errorf("volume %q has empty storageClassName and no default class: %w", vol.Name, err)
		}
		name = def.Name
	}
	var sc types.StorageClass
	if err := c.store.Get(ctx, types.ResourceTypeStorageClass, "", name, &sc); err != nil {
		return nil, fmt.Errorf("get storage class %q: %w", name, err)
	}
	return &sc, nil
}

// findDefaultStorageClass returns the single StorageClass with Default:true
// in the store. Returns an error if zero or more than one are flagged.
func (c *volumeController) findDefaultStorageClass(ctx context.Context) (*types.StorageClass, error) {
	var all []types.StorageClass
	if err := c.store.List(ctx, types.ResourceTypeStorageClass, "", &all); err != nil {
		return nil, fmt.Errorf("list storage classes: %w", err)
	}
	var found *types.StorageClass
	for i := range all {
		if !all[i].Default {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple storage classes marked Default (%q and %q); fix one", found.Name, all[i].Name)
		}
		found = &all[i]
	}
	if found == nil {
		return nil, fmt.Errorf("no storage class marked Default")
	}
	return found, nil
}

// runStorageClassWatch listens for StorageClass changes and enforces the
// at-most-one-Default invariant. When a Create or Update event lands a
// class with Default:true, all other Default classes are flipped to
// false so the most recent write wins atomically. Self-writes are
// suppressed via internalSCUpdates so the demotion does not loop.
func (c *volumeController) runStorageClassWatch(initial <-chan store.WatchEvent) {
	defer c.wg.Done()
	watchCh := initial
	for {
		select {
		case <-c.ctx.Done():
			return
		case ev, ok := <-watchCh:
			if !ok {
				next, err := c.store.Watch(c.ctx, types.ResourceTypeStorageClass, "")
				if err != nil {
					if errors.Is(c.ctx.Err(), context.Canceled) {
						return
					}
					c.logger.Error("StorageClass watch restart failed",
						log.Err(err))
					time.Sleep(2 * time.Second)
					continue
				}
				watchCh = next
				continue
			}
			if c.isSelfStorageClassUpdate(ev) {
				continue
			}
			if ev.Type != store.WatchEventCreated && ev.Type != store.WatchEventUpdated {
				continue
			}
			sc, ok := ev.Resource.(*types.StorageClass)
			if !ok || sc == nil || !sc.Default {
				continue
			}
			if err := c.enforceDefaultStorageClassUniqueness(c.ctx, sc.Name); err != nil {
				c.logger.Error("Failed to enforce Default StorageClass uniqueness",
					log.Str("trigger", sc.Name),
					log.Err(err))
			}
		}
	}
}

// applyDefaultStorageClassConfig honours the runefile
// [storage].defaultStorageClass knob:
//
//   - nil pointer: leave whatever Default state the store currently has.
//     Built-in seeding may have just installed `local` Default:true.
//   - pointer to non-empty name: ensure that named class exists and
//     carries Default:true. Returns an error if the named class is
//     missing (the operator chose a class we don't know about).
//   - pointer to empty string: explicit "no cluster default" \u2014 demote
//     every Default:true class to Default:false. Volumes with empty
//     storageClassName will then surface a clear resolution error.
//
// The uniqueness enforcer is the second pass; it demotes any
// duplicates this step might leave behind.
func (c *volumeController) applyDefaultStorageClassConfig(ctx context.Context) error {
	if c.defaultStorageClass == nil {
		return nil
	}
	target := *c.defaultStorageClass

	if target == "" {
		// Disable the cluster default entirely.
		var all []types.StorageClass
		if err := c.store.List(ctx, types.ResourceTypeStorageClass, "", &all); err != nil {
			return fmt.Errorf("list storage classes: %w", err)
		}
		for i := range all {
			if !all[i].Default {
				continue
			}
			cleared := all[i]
			cleared.Default = false
			cleared.UpdatedAt = time.Now().UTC()
			c.markSelfStorageClassUpdate(cleared.Name)
			if err := c.store.Update(ctx, types.ResourceTypeStorageClass, "", cleared.Name, &cleared); err != nil {
				return fmt.Errorf("clear default on storage class %q: %w", cleared.Name, err)
			}
			c.logger.Info("Cleared Default flag (storage.defaultStorageClass=\"\")",
				log.Str("name", cleared.Name))
		}
		return nil
	}

	// Promote the named class to Default:true.
	var sc types.StorageClass
	if err := c.store.Get(ctx, types.ResourceTypeStorageClass, "", target, &sc); err != nil {
		return fmt.Errorf("get configured default storage class %q: %w", target, err)
	}
	if sc.Default {
		return nil
	}
	sc.Default = true
	sc.UpdatedAt = time.Now().UTC()
	c.markSelfStorageClassUpdate(sc.Name)
	if err := c.store.Update(ctx, types.ResourceTypeStorageClass, "", sc.Name, &sc); err != nil {
		return fmt.Errorf("promote storage class %q to Default: %w", sc.Name, err)
	}
	c.logger.Info("Promoted storage class to Default per runefile",
		log.Str("name", sc.Name))
	return nil
}

// enforceDefaultStorageClassUniqueness ensures at most one StorageClass
// has Default:true. When `keep` is non-empty it is treated as the
// canonical Default and any other Default class is flipped to false.
// When `keep` is empty (boot path), the most-recently-updated Default
// class wins so existing-default-survives-restart is the default
// behaviour.
func (c *volumeController) enforceDefaultStorageClassUniqueness(ctx context.Context, keep string) error {
	var all []types.StorageClass
	if err := c.store.List(ctx, types.ResourceTypeStorageClass, "", &all); err != nil {
		return fmt.Errorf("list storage classes: %w", err)
	}
	var defaults []*types.StorageClass
	for i := range all {
		if all[i].Default {
			defaults = append(defaults, &all[i])
		}
	}
	if len(defaults) <= 1 {
		return nil
	}
	winner := keep
	if winner == "" {
		// Pick the most-recently-updated default; ties broken by name
		// for determinism. UpdatedAt zero values sort first so a
		// freshly-created default still wins over a zeroed one.
		var w *types.StorageClass
		for _, sc := range defaults {
			if w == nil ||
				sc.UpdatedAt.After(w.UpdatedAt) ||
				(sc.UpdatedAt.Equal(w.UpdatedAt) && sc.Name < w.Name) {
				w = sc
			}
		}
		winner = w.Name
	}
	for _, sc := range defaults {
		if sc.Name == winner {
			continue
		}
		demoted := *sc
		demoted.Default = false
		demoted.UpdatedAt = time.Now().UTC()
		c.markSelfStorageClassUpdate(demoted.Name)
		if err := c.store.Update(ctx, types.ResourceTypeStorageClass, "", demoted.Name, &demoted); err != nil {
			return fmt.Errorf("demote default storage class %q: %w", demoted.Name, err)
		}
		c.logger.Info("Demoted duplicate Default storage class",
			log.Str("name", demoted.Name),
			log.Str("winner", winner))
	}
	return nil
}

func (c *volumeController) markSelfStorageClassUpdate(name string) {
	c.internalSCUpdates.Store(name, time.Now())
}

func (c *volumeController) isSelfStorageClassUpdate(ev store.WatchEvent) bool {
	if ev.Type != store.WatchEventUpdated {
		return false
	}
	if _, ok := c.internalSCUpdates.LoadAndDelete(ev.Name); ok {
		return true
	}
	return false
}

// seedBuiltInStorageClasses idempotently creates the built-in "local"
// (Default:true) and "local-host" StorageClass resources at controller
// boot if they don't already exist. Operators can override either by
// pre-creating a class with the same name (the existence check below skips
// the seed) or by creating their own Default class (the API server's
// at-most-one invariant will then flip ours off).
func (c *volumeController) seedBuiltInStorageClasses(ctx context.Context) error {
	now := time.Now()
	builtIns := []types.StorageClass{
		{
			Name:          "local",
			Driver:        "local",
			ReclaimPolicy: types.ReclaimPolicyRetain,
			Default:       true,
			Labels:        map[string]string{"rune.io/builtin": "true"},
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		{
			Name:          "local-host",
			Driver:        "local-host",
			ReclaimPolicy: types.ReclaimPolicyRetain,
			Labels:        map[string]string{"rune.io/builtin": "true"},
			CreatedAt:     now,
			UpdatedAt:     now,
		},
	}
	for i := range builtIns {
		sc := builtIns[i]
		var existing types.StorageClass
		if err := c.store.Get(ctx, types.ResourceTypeStorageClass, "", sc.Name, &existing); err == nil {
			// Already present — operator may have customised it; leave alone.
			continue
		}
		if err := c.store.Create(ctx, types.ResourceTypeStorageClass, "", sc.Name, &sc); err != nil {
			return fmt.Errorf("seed storage class %q: %w", sc.Name, err)
		}
		c.logger.Info("Seeded built-in storage class",
			log.Str("name", sc.Name),
			log.Str("driver", sc.Driver),
			log.Bool("default", sc.Default))
	}
	return nil
}

// driverFor returns a cached driver instance, building it on first use.
func (c *volumeController) driverFor(name string) (driver.Driver, error) {
	c.driverMu.RLock()
	d, ok := c.drivers[name]
	c.driverMu.RUnlock()
	if ok {
		return d, nil
	}

	c.driverMu.Lock()
	defer c.driverMu.Unlock()
	// Double-check after re-locking.
	if d, ok := c.drivers[name]; ok {
		return d, nil
	}
	cfg := c.driverConfigs[name]
	d, err := driver.New(name, cfg)
	if err != nil {
		return nil, err
	}
	c.drivers[name] = d
	return d, nil
}

func (c *volumeController) updateStatus(ctx context.Context, vol *types.Volume, status types.VolumeStatus, reason, message string) error {
	vol.Status = status
	vol.Reason = reason
	vol.Message = message
	vol.UpdatedAt = time.Now().UTC()
	c.markSelfUpdate(vol)
	if err := c.store.Update(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol); err != nil {
		return fmt.Errorf("update volume %s/%s: %w", vol.Namespace, vol.Name, err)
	}
	return nil
}

// markFailed transitions a volume to Failed. Distinct from updateStatus
// only so the call sites read clearly.
func (c *volumeController) markFailed(ctx context.Context, vol *types.Volume, reason, message string) error {
	c.logger.Error("Volume marked failed",
		log.Str("namespace", vol.Namespace),
		log.Str("name", vol.Name),
		log.Str("reason", reason),
		log.Str("message", message))
	return c.updateStatus(ctx, vol, types.VolumeStatusFailed, reason, message)
}

func (c *volumeController) markSelfUpdate(vol *types.Volume) {
	c.internalUpdates.Store(volumeKey(vol.Namespace, vol.Name), time.Now())
}

func (c *volumeController) isSelfUpdate(ev store.WatchEvent) bool {
	if ev.Type != store.WatchEventUpdated {
		return false
	}
	key := volumeKey(ev.Namespace, ev.Name)
	if _, ok := c.internalUpdates.LoadAndDelete(key); ok {
		return true
	}
	return false
}

func volumeKey(ns, name string) string { return ns + "/" + name }

// restoreFromSnapshot resolves the snapshot referenced by ref
// ("<namespace>/<name>"), validates it belongs to the same driver as the
// target volume, and invokes Driver.RestoreFromSnapshot.
func (c *volumeController) restoreFromSnapshot(
	ctx context.Context,
	target *types.Volume,
	class *types.StorageClass,
	merged map[string]string,
	ref string,
	d driver.Driver,
) (driver.VolumeHandle, error) {
	ns, name := splitNamespacedRef(ref)
	if ns == "" || name == "" {
		return "", fmt.Errorf("invalid restoreFromSnapshot reference %q (want <namespace>/<name>)", ref)
	}
	var snap types.Snapshot
	if err := c.store.Get(ctx, types.ResourceTypeSnapshot, ns, name, &snap); err != nil {
		return "", fmt.Errorf("get snapshot %s/%s: %w", ns, name, err)
	}
	if snap.Phase != types.SnapshotPhaseReady {
		return "", fmt.Errorf("snapshot %s/%s is in phase %q (must be Ready)", ns, name, snap.Phase)
	}
	if snap.Driver != "" && snap.Driver != class.Driver {
		return "", fmt.Errorf("snapshot %s/%s belongs to driver %q but target storage class uses %q",
			ns, name, snap.Driver, class.Driver)
	}
	sizeBytes, err := volumeSizeBytes(target)
	if err != nil {
		return "", err
	}
	opctx := driver.OpContext{
		StorageClass: class,
		Volume:       target,
		Parameters:   merged,
	}
	return d.RestoreFromSnapshot(ctx, opctx, driver.RestoreRequest{
		Source:       &snap,
		SourceHandle: driver.SnapshotHandle(snap.Handle),
		SizeBytes:    sizeBytes,
	})
}

// volumeSizeBytes parses Volume.Size into an int64 byte count for the
// driver layer. The empty string is allowed (returns 0) so drivers that
// don't care about size — host-path bind mounts, the `local` driver —
// keep working without a controller-side requirement to declare one.
// Drivers that need a non-zero size, like `do-volume`, validate
// internally and surface a clear error to the operator.
//
// Accepts the same string forms ParseMemory does: Kubernetes-quantity
// PascalCase units (Ki/Mi/Gi/Ti/Pi/Ei), decimal SI (K/M/G/T/P/E), and
// unitless integers (interpreted as bytes, matching the Quantity spec).
func volumeSizeBytes(vol *types.Volume) (int64, error) {
	if vol == nil || vol.Size == "" {
		return 0, nil
	}
	n, err := types.ParseMemory(vol.Size)
	if err != nil {
		return 0, fmt.Errorf("volume %s/%s: invalid size %q: %w", vol.Namespace, vol.Name, vol.Size, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("volume %s/%s: size %q parses to a negative byte count", vol.Namespace, vol.Name, vol.Size)
	}
	return n, nil
}

// splitNamespacedRef parses "<namespace>/<name>" into its parts. Returns
// empty strings if the input isn't well-formed.
func splitNamespacedRef(ref string) (string, string) {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '/' {
			return ref[:i], ref[i+1:]
		}
	}
	return "", ""
}

// mergeParameters layers Volume.Parameters on top of StorageClass.Parameters,
// matching the design's "user-supplied wins for the same key" rule.
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

// reclaimParameters builds the Parameters map for OpContext during a
// reclaim Delete. Resolution order:
//
//  1. Live class still around → merge(class.Parameters, vol.Parameters).
//     User-driven overrides on the volume win over class defaults, same
//     as the Provision path.
//  2. Class gone (orphan reclaim) but the volume carries a
//     DriverParameters snapshot taken at Provision time → use that
//     merged with the volume's live Parameters so post-provision overrides
//     still apply. Drivers like do-volume need region / auth refs here.
//  3. Neither → volume-local Parameters only. Best-effort; drivers
//     that strictly require class config fail with their own error,
//     and the operator can break the bind by hand.
//
// See RUNE-200 PR 2.
func reclaimParameters(class *types.StorageClass, vol *types.Volume) map[string]string {
	if vol == nil {
		return map[string]string{}
	}
	if class != nil {
		return mergeParameters(class.Parameters, vol.Parameters)
	}
	if len(vol.DriverParameters) > 0 {
		return mergeParameters(vol.DriverParameters, vol.Parameters)
	}
	return mergeParameters(nil, vol.Parameters)
}

// copyStringMap returns an independent copy of m so the caller can stash
// it without worrying about subsequent mutations to the source.
func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
