// persisted event emission for service and update transitions.

package reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
)

// SetEventLog wires the persisted event log. Nil-safe.
func (r *Reconciler) SetEventLog(eventLog events.EventLog) { r.events = eventLog }

// Well-known event reasons for the update lifecycle. Kept in the "update"
// vocabulary the spec field uses — operators read `updateStrategy`, so they
// should not have to learn a second word for the same thing.
const (
	eventUpdateStarted   = "UpdateStarted"
	eventInstanceRetired = "InstanceRetired"
	eventUpdateHolding   = "UpdateHolding"
	eventUpdateStalled   = "UpdateStalled"
	eventUpdateComplete  = "UpdateComplete"
)

// emitService records a service-scoped event. Best-effort and nil-safe.
func (r *Reconciler) emitService(ctx context.Context, service *types.Service, level types.EventLevel, reason, message string) {
	if r.events == nil || service == nil {
		return
	}
	if err := r.events.Emit(ctx, types.Event{
		Namespace: service.Namespace,
		Kind:      "Service",
		Name:      service.Name,
		UID:       service.ID,
		Level:     level,
		Reason:    reason,
		Message:   message,
	}); err != nil {
		r.logger.Warn("Failed to emit service event",
			log.Str("service", service.Name), log.Err(err))
	}
}

// emitUpdateTransitions records the update lifecycle by comparing the block we
// are about to persist against the one already stored. Only transitions are
// emitted — a held update ticks every 30s and must not produce an event each
// time, which is the difference between a readable timeline and log spam.
func (r *Reconciler) emitUpdateTransitions(ctx context.Context, service *types.Service, prev, next *types.UpdateStatus, stalled bool) {
	if r.events == nil {
		return
	}

	switch {
	case prev == nil && next != nil:
		r.emitService(ctx, service, types.EventLevelInfo, eventUpdateStarted,
			fmt.Sprintf("updating %d instance(s) to template generation %d",
				next.Outdated, next.TemplateGeneration))

	case prev != nil && next == nil:
		r.emitService(ctx, service, types.EventLevelInfo, eventUpdateComplete,
			fmt.Sprintf("update to template generation %d complete; %d instance(s) serving",
				prev.TemplateGeneration, prev.Available))

	case prev != nil && next != nil:
		// A new template landed mid-update: report the old one finishing and
		// the new one starting rather than silently switching targets.
		if prev.TemplateGeneration != next.TemplateGeneration {
			r.emitService(ctx, service, types.EventLevelInfo, eventUpdateStarted,
				fmt.Sprintf("superseded by template generation %d; %d instance(s) outdated",
					next.TemplateGeneration, next.Outdated))
			return
		}
		if stalled && service.StatusReason != types.ServiceReasonUpdateStalled {
			// Edge-triggered: only when the service is crossing INTO stalled.
			r.emitService(ctx, service, types.EventLevelWarn, eventUpdateStalled,
				fmt.Sprintf("no progress for %s: %s",
					time.Since(next.LastProgressAt).Round(time.Second), next.Message))
			return
		}
		// Holding is edge-triggered on the message changing, so a steady hold
		// is recorded once rather than on every resync tick.
		if !stalled && next.Message != "" && next.Message != prev.Message {
			r.emitService(ctx, service, types.EventLevelInfo, eventUpdateHolding, next.Message)
		}
	}
}
