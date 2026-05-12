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
	return &volumeController{
		store:         opts.Store,
		logger:        opts.Logger.WithComponent("volume-controller"),
		driverConfigs: opts.DriverConfigs,
		drivers:       make(map[string]driver.Driver),
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
	if err := c.enforceDefaultStorageClassUniqueness(c.ctx, ""); err != nil {
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
// Failed) — those transitions belong to the binder/claim system, not here.
func (c *volumeController) reconcile(ctx context.Context, vol *types.Volume) error {
	switch vol.Status {
	case "", types.VolumeStatusPending, types.VolumeStatusProvisioning:
		// fall through
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

	handle, err := d.Provision(ctx, driver.ProvisionRequest{
		Volume:           vol,
		StorageClass:     class,
		MergedParameters: merged,
		SizeBytes:        0, // size parsing lives in the binder; drivers fall back to a sane default for now
	})
	if err != nil {
		return c.markFailed(ctx, vol, "ProvisionFailed", err.Error())
	}

	vol.Handle = string(handle)
	return c.updateStatus(ctx, vol, types.VolumeStatusAvailable, "", "")
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
	if class, err := c.resolveStorageClass(ctx, vol); err == nil {
		driverName = class.Driver
	}
	if driverName == "" {
		c.logger.Warn("Cannot resolve driver for reclaim; leaving handle in place",
			log.Str("namespace", vol.Namespace),
			log.Str("name", vol.Name),
			log.Str("handle", vol.Handle))
		return nil
	}
	d, err := c.driverFor(driverName)
	if err != nil {
		return fmt.Errorf("reclaim: driver %q: %w", driverName, err)
	}
	if err := d.Delete(ctx, driver.VolumeHandle(vol.Handle)); err != nil {
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
