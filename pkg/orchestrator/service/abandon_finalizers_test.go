package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type eventCapture struct {
	mu     sync.Mutex
	events []types.Event
}

func (l *eventCapture) Emit(_ context.Context, e types.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return nil
}
func (l *eventCapture) ListByResource(context.Context, string, string, string, int) ([]types.Event, error) {
	return nil, nil
}
func (l *eventCapture) ListSince(context.Context, int64, int) ([]types.Event, error) { return nil, nil }
func (l *eventCapture) LoadCursor(context.Context, string) (int64, error)            { return 0, nil }
func (l *eventCapture) SaveCursor(context.Context, string, int64) error              { return nil }

func TestAbandonFinalizers_RejectedUnlessBeingDeleted(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)
	svc := createTestService(ctx, t, testStore, "live")

	_, err := controller.DeleteService(ctx, &types.DeletionRequest{
		Namespace: svc.Namespace, Name: svc.Name, AbandonFinalizers: true,
	})
	require.Error(t, err)
	assert.True(t, types.IsValidationError(err), "the API maps this to FailedPrecondition")
	assert.Contains(t, err.Error(), "rune delete service live")

	var got types.Service
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, &got))
	assert.Nil(t, got.Metadata.DeletionTimestamp, "a live service must not be touched")
}

// A teardown stuck on its finalizers can be cleared by the operator; the
// reconciler then removes the record, and what was abandoned is named.
func TestAbandonFinalizers_ClearsAndRemovesRecord(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)
	sc := controller.(*serviceController)
	events := &eventCapture{}
	sc.SetEventLog(events)

	svc := createTestService(ctx, t, testStore, "stuck")
	now := time.Now()
	require.NoError(t, testStore.UpdateFunc(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc, func() error {
		svc.Metadata.DeletionTimestamp = &now
		svc.Metadata.Finalizers = []types.FinalizerType{types.FinalizerTypeVolumeCleanup}
		svc.Status = types.ServiceStatusDeleted
		return nil
	}))
	vol := &types.Volume{Name: "data-stuck-0", Namespace: svc.Namespace, OwnerService: svc.Namespace + "/" + svc.Name}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	dry, err := controller.DeleteService(ctx, &types.DeletionRequest{
		Namespace: svc.Namespace, Name: svc.Name, AbandonFinalizers: true, DryRun: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "dry_run", dry.Status)
	var got types.Service
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, &got))
	assert.NotEmpty(t, got.Metadata.Finalizers, "a dry run must not clear anything")

	require.NoError(t, controller.Start(ctx))
	defer func() { _ = controller.Stop() }()

	resp, err := controller.DeleteService(ctx, &types.DeletionRequest{
		Namespace: svc.Namespace, Name: svc.Name, AbandonFinalizers: true,
	})
	require.NoError(t, err)
	require.Len(t, resp.Warnings, 1)
	assert.Contains(t, resp.Warnings[0], string(types.FinalizerTypeVolumeCleanup))
	assert.Contains(t, resp.Warnings[0], "data-stuck-0")

	require.Eventually(t, func() bool {
		var got types.Service
		return store.IsNotFoundError(testStore.Get(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, &got))
	}, 10*time.Second, 20*time.Millisecond, "the reconciler removes the record once its finalizers are cleared")

	events.mu.Lock()
	defer events.mu.Unlock()
	require.Len(t, events.events, 1)
	assert.Equal(t, "FinalizersAbandoned", events.events[0].Reason)
	assert.Equal(t, types.EventLevelWarn, events.events[0].Level)
	assert.Contains(t, events.events[0].Message, "data-stuck-0")
}
