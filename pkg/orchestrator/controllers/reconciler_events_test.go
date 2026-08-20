package controllers

// RUNE-042 Phase 6: the update lifecycle must leave a record. Service.Update
// is cleared the moment an update converges, so events are the only
// after-the-fact answer to "what did that deploy do?" — and both design
// reviews put "no events at all" at the top of the diagnosis gap.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingEventLog captures emitted events for assertions.
type recordingEventLog struct {
	mu     sync.Mutex
	events []types.Event
}

func (l *recordingEventLog) Emit(_ context.Context, e types.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return nil
}
func (l *recordingEventLog) ListByResource(context.Context, string, string, string, int) ([]types.Event, error) {
	return nil, nil
}
func (l *recordingEventLog) ListSince(context.Context, int64, int) ([]types.Event, error) {
	return nil, nil
}
func (l *recordingEventLog) LoadCursor(context.Context, string) (int64, error) { return 0, nil }
func (l *recordingEventLog) SaveCursor(context.Context, string, int64) error   { return nil }

func (l *recordingEventLog) reasons() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.events))
	for _, e := range l.events {
		out = append(out, e.Reason)
	}
	return out
}

func (l *recordingEventLog) countOf(reason string) int {
	n := 0
	for _, r := range l.reasons() {
		if r == reason {
			n++
		}
	}
	return n
}

func (l *recordingEventLog) messageFor(reason string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e.Reason == reason {
			return e.Message
		}
	}
	return ""
}

// An update must narrate itself: started, each retirement, and completion.
func TestEvents_UpdateLifecycleIsRecorded(t *testing.T) {
	ctx, st, tr, r := updateFixture(t)
	log := &recordingEventLog{}
	r.SetEventLog(log)

	svc := updateSvc(t, ctx, st, 2, 1)
	seedInstance(t, ctx, st, tr, svc, "api-a", 1, 30*time.Second)
	seedInstance(t, ctx, st, tr, svc, "api-b", 1, 20*time.Second)

	svc.Metadata.Generation = 2
	svc.Metadata.TemplateGeneration = 2
	require.NoError(t, st.Update(ctx, types.ResourceTypeService, "default", svc.Name, svc))

	// Tick 1: the update starts.
	require.NoError(t, r.reconcileService(ctx, svc))
	assert.Equal(t, 1, log.countOf(eventUpdateStarted), "an update must announce itself")
	assert.Contains(t, log.messageFor(eventUpdateStarted), "template generation 2")

	// Drive to convergence.
	for tick := 0; tick < 12; tick++ {
		var fresh types.Service
		require.NoError(t, st.Get(ctx, types.ResourceTypeService, "default", "api", &fresh))
		require.NoError(t, r.reconcileService(ctx, &fresh))
		for _, inst := range liveInstances(t, ctx, st) {
			i := inst
			if i.Status == types.InstanceStatusPending || i.Status == types.InstanceStatusStarting {
				i.Status = types.InstanceStatusRunning
				tr.StatusResults[i.ID] = types.InstanceStatusRunning
			}
			settled := time.Now().Add(-time.Minute)
			i.LastTransitionAt = &settled
			require.NoError(t, st.Update(ctx, types.ResourceTypeInstance, "default", i.ID, &i))
		}
	}

	assert.GreaterOrEqual(t, log.countOf(eventInstanceRetired), 1,
		"each retirement must be recorded with its reason")
	assert.Equal(t, 1, log.countOf(eventUpdateComplete),
		"completion must be recorded exactly once — it is the only record left once Update is cleared")
}

// A held update ticks every resync. Emitting on each one would bury the
// timeline in noise, so holding is edge-triggered on the message changing.
func TestEvents_HoldingIsNotEmittedEveryTick(t *testing.T) {
	ctx, st, tr, r := updateFixture(t)
	log := &recordingEventLog{}
	r.SetEventLog(log)

	svc := updateSvc(t, ctx, st, 3, 1)
	for i := 0; i < 3; i++ {
		seedInstance(t, ctx, st, tr, svc, fmt.Sprintf("api-%d", i), 1, time.Duration(30-i*10)*time.Second)
	}
	svc.Metadata.Generation = 2
	svc.Metadata.TemplateGeneration = 2
	require.NoError(t, st.Update(ctx, types.ResourceTypeService, "default", svc.Name, svc))

	// Reconcile repeatedly WITHOUT promoting anything: the update creates its
	// replacement and then holds, unchanged, forever.
	for tick := 0; tick < 8; tick++ {
		var fresh types.Service
		require.NoError(t, st.Get(ctx, types.ResourceTypeService, "default", "api", &fresh))
		require.NoError(t, r.reconcileService(ctx, &fresh))
	}

	assert.LessOrEqual(t, log.countOf(eventUpdateHolding), 2,
		"a steady hold must be recorded once, not once per resync tick: got %v", log.reasons())
}

// The stall message must keep the planner's sentence — "no progress" alone
// tells an operator nothing they did not already know.
func TestStalledMessage_KeepsThePlannerReason(t *testing.T) {
	assert.Equal(t, "update made no progress within the stall deadline",
		stalledMessage(nil))
	assert.Equal(t,
		"update made no progress within the stall deadline: waiting for replacements to become ready",
		stalledMessage(&types.UpdateStatus{Message: "waiting for replacements to become ready"}))
}
