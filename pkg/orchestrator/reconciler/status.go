// service status roll-up and update status computation.

package reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/log"
	instancectl "github.com/runestack/rune/pkg/orchestrator/instance"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// updateServiceStatus updates a service's status based on its instances
func (r *Reconciler) updateServiceStatus(ctx context.Context, service *types.Service) error {
	// Don't change the status if service is already marked as deleted
	if service.Status == types.ServiceStatusDeleted {
		return nil
	}

	// Fetch the latest instance data directly from the store
	instanceData, err := r.getServiceInstances(ctx, service)
	if err != nil {
		return fmt.Errorf("failed to get latest instances for status update: %w", err)
	}

	var newStatus types.ServiceStatus
	var newReason, newMessage string
	var upd *types.UpdateStatus
	var stalled bool

	if len(instanceData.Instances) == 0 {
		// No instances yet
		newStatus = types.ServiceStatusPending
	} else {
		// Count instances in each state
		pending := 0
		running := 0
		failed := 0

		// Track the worst instance so we can roll its StatusMessage up
		// to the service. Failed wins over Pending wins over Running.
		// First failed instance encountered is enough — operators get
		// one concrete sentence to act on.
		var worstFailed, worstPending *types.Instance

		for i := range instanceData.Instances {
			instance := &instanceData.Instances[i]
			switch instance.Status {
			case types.InstanceStatusPending, types.InstanceStatusCreated, types.InstanceStatusStarting:
				pending++
				if worstPending == nil {
					worstPending = instance
				}
			case types.InstanceStatusRunning:
				running++
			case types.InstanceStatusFailed, types.InstanceStatusExited, types.InstanceStatusUnknown, types.InstanceStatusStalled:
				// Stalled is the PR2 terminal "create retries
				// exhausted, operator must act" state. Roll it up as
				// Failed so `rune get services` doesn't show
				// misleading Pending for a slot that will never
				// self-recover.
				failed++
				if worstFailed == nil {
					worstFailed = instance
				}
			}
		}

		r.logger.Debug("Instance status counts",
			log.Str("service", service.Name),
			log.Int("pending", pending),
			log.Int("running", running),
			log.Int("failed", failed),
			log.Int("total", len(instanceData.Instances)))

		// Update progress + stall detection (RUNE-042 §8.2/§7.3). nil means
		// no update is in flight.
		upd, stalled = r.computeUpdateStatus(ctx, service, instanceData)

		// Determine overall service status.
		//
		// Stopping wins over Running/Deploying/Pending whenever the desired
		// scale is below the current instance count — the reconciler is
		// actively tearing instances down. Without this, `rune stop` and the
		// drain phase of `rune restart` show "Running" the whole time the
		// old instance is still around, which reads as a contradiction next
		// to the desired scale of 0.
		//
		// Failed still wins over Stopping: an instance that's failing to
		// terminate cleanly is more important to surface than the fact that
		// we're trying to terminate it.
		//
		// Two of these rules break under an update and are guarded (RUNE-042
		// §8.2):
		//
		//   - An update legitimately runs at Scale+extra instances, so the
		//     count-exceeds-scale test would report the whole update as
		//     "Stopping" — wrong, and alarming next to a healthy deploy.
		//   - One replacement failing to start must not flip a service whose
		//     old instances are all serving. While an update is in flight a
		//     failed instance keeps the service Deploying/Updating until the
		//     stall deadline, and only then becomes Failed/UpdateStalled.
		//     A failed instance with NO update in flight keeps the old
		//     behaviour exactly.
		updating := upd != nil
		switch {
		case failed > 0 && (!updating || stalled):
			newStatus = types.ServiceStatusFailed
			if stalled {
				newReason = types.ServiceReasonUpdateStalled
				newMessage = stalledMessage(upd)
			} else if worstFailed != nil {
				newReason = types.DeriveServiceReason(worstFailed.Status, worstFailed.StatusMessage)
				newMessage = worstFailed.StatusMessage
			}
		case stalled:
			newStatus = types.ServiceStatusFailed
			newReason = types.ServiceReasonUpdateStalled
			newMessage = stalledMessage(upd)
		case updating:
			newStatus = types.ServiceStatusDeploying
			newReason = types.ServiceReasonUpdating
			newMessage = upd.Message
		case service.Scale < len(instanceData.Instances):
			newStatus = types.ServiceStatusStopping
		case pending > 0:
			newStatus = types.ServiceStatusDeploying
			if worstPending != nil && worstPending.StatusMessage != "" {
				newMessage = worstPending.StatusMessage
			}
		case running == len(instanceData.Instances):
			newStatus = types.ServiceStatusRunning
		default:
			newStatus = types.ServiceStatusPending
		}
	}

	// The generation we just reconciled. Recording it in ObservedGeneration is
	// how the service controller later recognises a status-only update and skips
	// a redundant reconcile (RFC #129 Phase 2). Capture the snapshot generation,
	// not the fresh one — if the desired state changed again mid-reconcile, the
	// mismatch must persist so the next event reconciles the newer generation.
	reconciledGen := int64(0)
	if service.Metadata != nil {
		reconciledGen = service.Metadata.Generation
	}

	statusChanged := service.Status != newStatus ||
		service.StatusReason != newReason ||
		service.StatusMessage != newMessage ||
		updateStatusChanged(service.Update, upd)
	// Even when the status text is unchanged, we must advance ObservedGeneration
	// so a scale-up that leaves the service "Running" (e.g. `rune restart`'s
	// scale-back-up) doesn't look unreconciled forever and reconcile-loop.
	genNeedsObserve := service.Metadata == nil || service.Metadata.ObservedGeneration != reconciledGen

	if statusChanged || genNeedsObserve {
		if statusChanged {
			r.logger.Info("Updating service status",
				log.Str("service", service.Name),
				log.Str("from", string(service.Status)),
				log.Str("to", string(newStatus)),
				log.Str("reason", newReason),
				// Without this the planner's actual sentence ("waiting for
				// replacements to become ready") never reached the log, and a
				// held update was undiagnosable from logs alone.
				log.Str("message", newMessage))
		}

		// Apply ONLY the status fields (+ ObservedGeneration), atomically, on the
		// freshly-read service. updateServiceStatus runs at the end of a reconcile
		// cycle on a `service` snapshot taken at the top; a concurrent scale write
		// (e.g. `rune restart`'s 0→1 leg via the scaling controller) can land in
		// between. UpdateFunc's CAS guarantees this write never clobbers that
		// Scale (the "restart goes 1→0 and hangs" bug) (RFC #129).
		var fresh types.Service
		err := r.store.UpdateFunc(ctx, types.ResourceTypeService, service.Namespace, service.Name, &fresh, func() error {
			// Don't resurrect a service that was deleted out from under us.
			if fresh.Status == types.ServiceStatusDeleted {
				return store.ErrSkipUpdate
			}
			if fresh.Metadata == nil {
				fresh.Metadata = &types.ServiceMetadata{}
			}
			// Skip only if BOTH the status and the observed generation already
			// match — otherwise another writer beat us but we still owe the
			// ObservedGeneration bump (or vice versa).
			statusMatches := fresh.Status == newStatus &&
				fresh.StatusReason == newReason &&
				fresh.StatusMessage == newMessage &&
				!updateStatusChanged(fresh.Update, upd)
			if statusMatches && fresh.Metadata.ObservedGeneration == reconciledGen {
				return store.ErrSkipUpdate
			}
			fresh.Status = newStatus
			fresh.StatusReason = newReason
			fresh.StatusMessage = newMessage
			// Update rides this same CAS write. A separate write would
			// reintroduce exactly the clobber-a-concurrent-scale race RFC #129
			// closed. nil clears it — an update that has converged leaves no
			// stale block behind, because Verify and the CLI spinner both key
			// off the transition back to Running.
			fresh.Update = upd
			fresh.Metadata.ObservedGeneration = reconciledGen
			return nil
		}, store.WithReconciler())
		if err != nil {
			return fmt.Errorf("failed to update service status: %w", err)
		}
		r.emitUpdateTransitions(ctx, service, service.Update, upd, stalled)

		// Reflect the persisted fields on the caller's copy for consistency.
		// Clone Metadata rather than writing through it: the pointer may be
		// shared with watch-event consumers and other store readers
		// (TestStore's copies are shallow; Badger round-trips, but the
		// discipline holds either way) — an in-place write would race with
		// their reads. The top-level status fields are our own struct copy.
		service.Status = newStatus
		service.StatusReason = newReason
		service.StatusMessage = newMessage
		service.Update = upd
		md := types.ServiceMetadata{}
		if service.Metadata != nil {
			md = *service.Metadata
		}
		md.ObservedGeneration = reconciledGen
		service.Metadata = &md
	}

	return nil
}

// computeUpdateStatus derives the in-flight update block for a service, and
// reports whether the update has stalled (RUNE-042 §7.3/§8.2).
//
// Returns (nil, false) when no update is running — every live instance is at
// the current template — which is what clears Service.Update on completion.
//
// Progress is measured against the PERSISTED previous block, so LastProgressAt
// survives a runed restart mid-update and the stall clock is not reset by one.
// A dependency gate pauses the clock rather than letting it expire: an update
// frozen because a dependency is unready is a healthy hold, not a stall.
func (r *Reconciler) computeUpdateStatus(ctx context.Context, service *types.Service, instanceData *ServiceInstanceData) (*types.UpdateStatus, bool) {
	params := service.ResolveUpdateParams()
	now := time.Now()

	var templateGen int64
	if service.Metadata != nil {
		templateGen = service.Metadata.TemplateGeneration
	}

	next := &types.UpdateStatus{
		TemplateGeneration: templateGen,
		Desired:            service.Scale,
	}
	for i := range instanceData.Instances {
		inst := &instanceData.Instances[i]
		verdict := r.instanceController.ClassifyInstance(ctx, inst, service)
		v := newInstanceView(inst, verdict.Class, params.MinReady, now)

		if verdict.Class == instancectl.CompatOutdated && !v.Terminating {
			// Mirror the planner, which excludes Terminating instances from
			// liveOutdated. Counting one here without the planner being able
			// to retire it means Outdated can never reach zero: the update
			// reads as permanently in flight, never progresses, and lands on
			// UpdateStalled forever — with every later `release --atomic` on
			// the service failing verify. A record stranded in Terminating
			// (runed killed mid-teardown) is reaped by reapStuckTerminating
			// instead.
			next.Outdated++
		} else if verdict.Class == instancectl.CompatOK {
			// "Updated" counts instances at the current template. A repaired
			// instance lands there too — CreateInstance stamps the current
			// TemplateGeneration — so converging through crash-replacements
			// registers as progress rather than reading as a permanent stall
			// (§8.2).
			next.Updated++
			if v.Ready {
				next.UpdatedReady++
			}
		}
		if v.serving() {
			next.Available++
		}
	}

	// No outdated instances left: the update is done (or never started).
	if next.Outdated == 0 {
		return nil, false
	}

	prev := service.Update
	next.StartedAt = now
	next.LastProgressAt = now
	if prev != nil && prev.TemplateGeneration == templateGen {
		next.StartedAt = prev.StartedAt
		next.LastProgressAt = prev.LastProgressAt
		// Ask BEFORE raising the marks, or every tick would trivially match
		// its own peak.
		if prev.Progressed(next) {
			next.LastProgressAt = now
		}
	}
	next.CarryPeaksFrom(prev)

	// A dependency gate freezes instance creation entirely, so the stall
	// clock would expire on a service that is behaving correctly.
	if len(service.Dependencies) > 0 {
		if ready, err := r.dependenciesReady(ctx, service); err == nil && !ready {
			next.LastProgressAt = now
			next.Message = "blocked on dependency"
			return next, false
		}
	}

	// The planner's own sentence is the most useful thing to show ("waiting
	// for replacements to become ready", "retiring 1 instance(s)"), so reuse
	// it rather than inventing a second vocabulary.
	if next.Message == "" {
		views := make([]instanceView, 0, len(instanceData.Instances))
		for i := range instanceData.Instances {
			inst := &instanceData.Instances[i]
			verdict := r.instanceController.ClassifyInstance(ctx, inst, service)
			views = append(views, newInstanceView(inst, verdict.Class, params.MinReady, now))
		}
		plan := planUpdate(updateInput{Scale: service.Scale, Params: params, Instances: views, Now: now})
		next.Message = plan.Reason
	}

	stalled := now.Sub(next.LastProgressAt) > params.StallDeadline
	return next, stalled
}

// stalledMessage explains a stall without throwing away the planner's own
// sentence, which is the part that says WHY nothing is moving.
func stalledMessage(upd *types.UpdateStatus) string {
	base := "update made no progress within the stall deadline"
	if upd == nil || upd.Message == "" {
		return base
	}
	return base + ": " + upd.Message
}

// updateStatusChanged reports whether the update block needs persisting.
// Timestamps are deliberately excluded from the comparison except when they
// carry news: LastProgressAt moves only on real progress, so comparing the
// counters plus the message avoids a store write on every reconcile tick of a
// held update.
func updateStatusChanged(a, b *types.UpdateStatus) bool {
	switch {
	case a == nil && b == nil:
		return false
	case a == nil || b == nil:
		return true
	}
	return a.TemplateGeneration != b.TemplateGeneration ||
		a.Desired != b.Desired ||
		a.Updated != b.Updated ||
		a.UpdatedReady != b.UpdatedReady ||
		a.Available != b.Available ||
		a.Outdated != b.Outdated ||
		a.Message != b.Message ||
		!a.LastProgressAt.Equal(b.LastProgressAt)
}
