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

// SnapshotController owns the lifecycle of Snapshot resources: it
// watches the store, resolves the source Volume + StorageClass +
// driver, drives Pending → Creating → Ready, and on Deleting calls
// Driver.DeleteSnapshot before removing the row.
//
// Introduced in RUNE-071 (Slice 10a).
type SnapshotController interface {
	Start(ctx context.Context) error
	Stop() error
}

// SnapshotControllerOptions configures the controller.
type SnapshotControllerOptions struct {
	Store         store.Store
	Logger        log.Logger
	DriverConfigs map[string]map[string]any
}

type snapshotController struct {
	store         store.Store
	logger        log.Logger
	driverConfigs map[string]map[string]any

	driverMu sync.RWMutex
	drivers  map[string]driver.Driver

	internalUpdates sync.Map

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewSnapshotController constructs a SnapshotController.
func NewSnapshotController(opts SnapshotControllerOptions) (SnapshotController, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("snapshot controller: store is required")
	}
	if opts.Logger == nil {
		return nil, fmt.Errorf("snapshot controller: logger is required")
	}
	return &snapshotController{
		store:         opts.Store,
		logger:        opts.Logger.WithComponent("snapshot-controller"),
		driverConfigs: opts.DriverConfigs,
		drivers:       make(map[string]driver.Driver),
	}, nil
}

func (c *snapshotController) Start(ctx context.Context) error {
	c.ctx, c.cancel = context.WithCancel(ctx)
	watchCh, err := c.store.Watch(c.ctx, types.ResourceTypeSnapshot, "")
	if err != nil {
		return fmt.Errorf("snapshot controller: watch: %w", err)
	}
	c.wg.Add(1)
	go c.run(watchCh)
	c.logger.Info("Snapshot controller started")
	return nil
}

func (c *snapshotController) Stop() error {
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
	c.logger.Info("Snapshot controller stopped")
	return nil
}

func (c *snapshotController) run(initial <-chan store.WatchEvent) {
	defer c.wg.Done()
	watchCh := initial
	for {
		select {
		case <-c.ctx.Done():
			return
		case ev, ok := <-watchCh:
			if !ok {
				next, err := c.store.Watch(c.ctx, types.ResourceTypeSnapshot, "")
				if err != nil {
					if errors.Is(c.ctx.Err(), context.Canceled) {
						return
					}
					c.logger.Error("Snapshot watch restart failed", log.Err(err))
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
				c.logger.Error("Snapshot event handling failed",
					log.Str("namespace", ev.Namespace),
					log.Str("name", ev.Name),
					log.Str("type", string(ev.Type)),
					log.Err(err))
			}
		}
	}
}

func (c *snapshotController) handle(ctx context.Context, ev store.WatchEvent) error {
	switch ev.Type {
	case store.WatchEventCreated, store.WatchEventUpdated:
		snap, ok := ev.Resource.(*types.Snapshot)
		if !ok {
			return fmt.Errorf("expected *types.Snapshot, got %T", ev.Resource)
		}
		return c.reconcile(ctx, snap)
	case store.WatchEventDeleted:
		// Two-phase delete is the controller-driven path (DeleteSnapshot
		// flips Phase=Deleting and the controller does the driver call,
		// then deletes the row itself). A bare DELETE event without
		// driver coordination only happens for snapshots that never
		// acquired a Handle, so there's nothing to clean up.
		return nil
	default:
		return nil
	}
}

func (c *snapshotController) reconcile(ctx context.Context, snap *types.Snapshot) error {
	switch snap.Phase {
	case "", types.SnapshotPhasePending:
		return c.create(ctx, snap)
	case types.SnapshotPhaseDeleting:
		return c.delete(ctx, snap)
	default:
		return nil
	}
}

func (c *snapshotController) create(ctx context.Context, snap *types.Snapshot) error {
	// Resolve the source Volume.
	var src types.Volume
	if err := c.store.Get(ctx, types.ResourceTypeVolume, snap.Namespace, snap.SourceVolume, &src); err != nil {
		return c.markFailed(ctx, snap, "SourceVolumeMissing",
			fmt.Sprintf("source volume %s/%s not found: %v", snap.Namespace, snap.SourceVolume, err))
	}
	if src.Handle == "" {
		return c.markFailed(ctx, snap, "SourceVolumeNotProvisioned",
			fmt.Sprintf("source volume %s/%s has no handle (status=%s)", snap.Namespace, snap.SourceVolume, src.Status))
	}
	// Resolve the StorageClass + driver.
	className := src.StorageClassName
	if className == "" {
		return c.markFailed(ctx, snap, "StorageClassMissing",
			"source volume has empty storageClassName")
	}
	var class types.StorageClass
	if err := c.store.Get(ctx, types.ResourceTypeStorageClass, "", className, &class); err != nil {
		return c.markFailed(ctx, snap, "StorageClassMissing",
			fmt.Sprintf("get storage class %q: %v", className, err))
	}
	d, err := c.driverFor(class.Driver)
	if err != nil {
		return c.markFailed(ctx, snap, "DriverUnavailable", err.Error())
	}
	if !d.Capabilities().Snapshots {
		return c.markFailed(ctx, snap, "SnapshotsUnsupported",
			fmt.Sprintf("driver %q does not support snapshots", class.Driver))
	}
	if snap.Driver == "" {
		snap.Driver = class.Driver
	}
	// Mark Creating before the call so an operator watching the
	// resource sees the transition even on a long-running snapshot.
	if err := c.updateStatus(ctx, snap, types.SnapshotPhaseCreating, "", ""); err != nil {
		return err
	}
	opctx := driver.OpContext{
		StorageClass: &class,
		Volume:       &src,
		Parameters:   mergeParameters(class.Parameters, src.Parameters),
	}
	handle, err := d.Snapshot(ctx, opctx, driver.SnapshotRequest{
		Handle:   driver.VolumeHandle(src.Handle),
		Snapshot: snap,
	})
	if err != nil {
		return c.markFailed(ctx, snap, "SnapshotFailed", err.Error())
	}
	snap.Handle = string(handle)
	return c.updateStatus(ctx, snap, types.SnapshotPhaseReady, "", "")
}

func (c *snapshotController) delete(ctx context.Context, snap *types.Snapshot) error {
	if snap.Handle == "" || snap.Driver == "" {
		// Nothing for the driver to do; just remove the row.
		return c.removeRow(ctx, snap)
	}
	d, err := c.driverFor(snap.Driver)
	if err != nil {
		return c.markFailed(ctx, snap, "DriverUnavailable", err.Error())
	}
	// Best-effort context build: drivers that don't need class config
	// (local) tolerate the empty Parameters map; cloud drivers may
	// already have their auth in runefile-level config and don't fail
	// here even if the source volume/class are gone. PR 2 will snapshot
	// the driver-relevant params onto the Snapshot row itself so this
	// remains correct after orphaning.
	opctx := c.snapshotOpContext(ctx, snap)
	if err := d.DeleteSnapshot(ctx, opctx, driver.SnapshotHandle(snap.Handle)); err != nil {
		return c.markFailed(ctx, snap, "DeleteSnapshotFailed", err.Error())
	}
	return c.removeRow(ctx, snap)
}

// snapshotOpContext builds an OpContext for snapshot-scoped operations
// (today: DeleteSnapshot). It attempts to resolve the source Volume +
// StorageClass for the best-possible Parameters map; on either lookup
// failure it returns an OpContext with the nil class / volume so the
// driver still has a well-formed value to read from. Drivers that
// require class context for snapshot ops are responsible for failing
// with a clear error.
func (c *snapshotController) snapshotOpContext(ctx context.Context, snap *types.Snapshot) driver.OpContext {
	opctx := driver.OpContext{Parameters: map[string]string{}}
	var src types.Volume
	if err := c.store.Get(ctx, types.ResourceTypeVolume, snap.Namespace, snap.SourceVolume, &src); err == nil {
		opctx.Volume = &src
		if src.StorageClassName != "" {
			var class types.StorageClass
			if err := c.store.Get(ctx, types.ResourceTypeStorageClass, "", src.StorageClassName, &class); err == nil {
				opctx.StorageClass = &class
				opctx.Parameters = mergeParameters(class.Parameters, src.Parameters)
			}
		}
	}
	return opctx
}

func (c *snapshotController) removeRow(ctx context.Context, snap *types.Snapshot) error {
	if err := c.store.Delete(ctx, types.ResourceTypeSnapshot, snap.Namespace, snap.Name); err != nil {
		return fmt.Errorf("delete snapshot %s/%s: %w", snap.Namespace, snap.Name, err)
	}
	c.logger.Info("Snapshot removed",
		log.Str("namespace", snap.Namespace),
		log.Str("name", snap.Name),
		log.Str("driver", snap.Driver))
	return nil
}

func (c *snapshotController) driverFor(name string) (driver.Driver, error) {
	c.driverMu.RLock()
	d, ok := c.drivers[name]
	c.driverMu.RUnlock()
	if ok {
		return d, nil
	}
	c.driverMu.Lock()
	defer c.driverMu.Unlock()
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

func (c *snapshotController) updateStatus(ctx context.Context, snap *types.Snapshot, phase types.SnapshotPhase, reason, message string) error {
	snap.Phase = phase
	snap.Reason = reason
	snap.Message = message
	snap.UpdatedAt = time.Now().UTC()
	c.markSelfUpdate(snap)
	if err := c.store.Update(ctx, types.ResourceTypeSnapshot, snap.Namespace, snap.Name, snap); err != nil {
		return fmt.Errorf("update snapshot %s/%s: %w", snap.Namespace, snap.Name, err)
	}
	return nil
}

func (c *snapshotController) markFailed(ctx context.Context, snap *types.Snapshot, reason, message string) error {
	c.logger.Error("Snapshot marked failed",
		log.Str("namespace", snap.Namespace),
		log.Str("name", snap.Name),
		log.Str("reason", reason),
		log.Str("message", message))
	return c.updateStatus(ctx, snap, types.SnapshotPhaseFailed, reason, message)
}

func (c *snapshotController) markSelfUpdate(snap *types.Snapshot) {
	c.internalUpdates.Store(volumeKey(snap.Namespace, snap.Name), time.Now())
}

func (c *snapshotController) isSelfUpdate(ev store.WatchEvent) bool {
	if ev.Type != store.WatchEventUpdated {
		return false
	}
	if _, ok := c.internalUpdates.LoadAndDelete(volumeKey(ev.Namespace, ev.Name)); ok {
		return true
	}
	return false
}
