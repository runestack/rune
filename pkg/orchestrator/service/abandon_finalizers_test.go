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

func tombstone(ctx context.Context, t *testing.T, st *store.TestStore, svc *types.Service, fins ...types.FinalizerType) {
	t.Helper()
	now := time.Now()
	require.NoError(t, st.UpdateFunc(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, svc, func() error {
		svc.Metadata.DeletionTimestamp = &now
		svc.Metadata.Finalizers = fins
		svc.Status = types.ServiceStatusDeleted
		return nil
	}))
}

// Instance cleanup never gets stuck, and abandoning it would leave instances
// that nothing reaps once the record is gone.
func TestAbandonFinalizers_RefusedDuringInstanceCleanup(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)
	svc := createTestService(ctx, t, testStore, "tearing")
	tombstone(ctx, t, testStore, svc, types.FinalizerTypeInstanceCleanup, types.FinalizerTypeVolumeCleanup)

	_, err := controller.DeleteService(ctx, &types.DeletionRequest{
		Namespace: svc.Namespace, Name: svc.Name, AbandonFinalizers: true,
	})
	require.Error(t, err)
	assert.True(t, types.IsValidationError(err))

	var got types.Service
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, &got))
	assert.Len(t, got.Metadata.Finalizers, 2, "nothing may be cleared")
}

// A teardown stuck on volume cleanup can be cleared by the operator; the
// owned volume is left in place and named, and the reconciler then removes
// the record. The controller only starts after the abandon, so the record's
// removal can only come from it.
func TestAbandonFinalizers_ClearsAndRemovesRecord(t *testing.T) {
	ctx, testStore, _, _, controller := setupTestServiceController(t)
	events := &eventCapture{}
	controller.(*serviceController).SetEventLog(events)

	svc := createTestService(ctx, t, testStore, "stuck")
	tombstone(ctx, t, testStore, svc, types.FinalizerTypeVolumeCleanup)
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

	resp, err := controller.DeleteService(ctx, &types.DeletionRequest{
		Namespace: svc.Namespace, Name: svc.Name, AbandonFinalizers: true,
	})
	require.NoError(t, err)
	require.Len(t, resp.Warnings, 1)
	assert.Contains(t, resp.Warnings[0], string(types.FinalizerTypeVolumeCleanup))
	assert.Contains(t, resp.Warnings[0], "data-stuck-0")
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, &got))
	assert.Empty(t, got.Metadata.Finalizers)

	require.NoError(t, controller.Start(ctx))
	defer func() { _ = controller.Stop() }()
	require.Eventually(t, func() bool {
		var got types.Service
		return store.IsNotFoundError(testStore.Get(ctx, types.ResourceTypeService, svc.Namespace, svc.Name, &got))
	}, 10*time.Second, 20*time.Millisecond, "the reconciler removes the record once its finalizers are cleared")

	var left types.Volume
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, &left),
		"an abandoned volume cleanup leaves the volume for the operator")

	events.mu.Lock()
	defer events.mu.Unlock()
	require.Len(t, events.events, 1)
	assert.Equal(t, "FinalizersAbandoned", events.events[0].Reason)
	assert.Equal(t, types.EventLevelWarn, events.events[0].Level)
	assert.Contains(t, events.events[0].Message, "data-stuck-0")
}
