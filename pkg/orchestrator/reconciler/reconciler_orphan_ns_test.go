package reconciler

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/log"
	instancectl "github.com/runestack/rune/pkg/orchestrator/instance"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/require"
)

// TestGetServiceInstances_OrphanScopedByNamespace is the regression test for
// the cross-namespace orphan-reaping churn: with `api` running in BOTH prod and
// staging on one runner, reconciling prod/api must NOT flag the running
// staging/api container as a prod orphan (which would delete it, and vice
// versa, forever). A genuine same-namespace orphan must still be flagged.
func TestGetServiceInstances_OrphanScopedByNamespace(t *testing.T) {
	ctx := context.Background()
	testStore := setupStore(t)
	testRunner := runner.NewTestRunner()
	rm := manager.NewTestRunnerManager(nil)
	rm.SetDockerRunner(testRunner)
	rm.SetProcessRunner(testRunner)
	ic := instancectl.NewController(testStore, rm, log.NewLogger())

	r := &Reconciler{
		store:              testStore,
		instanceController: ic,
		logger:             log.NewLogger().WithComponent("reconciler"),
	}

	// prod/api service + its one live instance (in the store).
	prodAPI := &types.Service{ID: "svc-prod-api", Name: "api", Namespace: "prod", Scale: 1}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeService, "prod", "api", prodAPI))
	liveProd := &types.Instance{
		ID: "prod-live-1", Name: "api-aaaaa", Namespace: "prod",
		ServiceName: "api", ServiceID: "svc-prod-api", Status: types.InstanceStatusRunning,
	}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "prod", liveProd.ID, liveProd))

	// What the RUNNER reports as running: the live prod instance, a
	// staging/api instance (different namespace, SAME service name), and a
	// genuine prod/api orphan (running but no store record).
	testRunner.Instances = map[string]*types.Instance{
		"prod-live-1":    {ID: "prod-live-1", Namespace: "prod", ServiceName: "api", Status: types.InstanceStatusRunning},
		"staging-live-1": {ID: "staging-live-1", Namespace: "staging", ServiceName: "api", Status: types.InstanceStatusRunning},
		"prod-orphan-1":  {ID: "prod-orphan-1", Namespace: "prod", ServiceName: "api", Status: types.InstanceStatusRunning},
	}

	data, err := r.getServiceInstances(ctx, prodAPI)
	require.NoError(t, err)

	got := map[string]bool{}
	for _, o := range data.OrphanedInstances {
		got[o.ID] = true
	}

	require.True(t, got["prod-orphan-1"], "a genuine same-namespace orphan must still be reaped")
	require.False(t, got["staging-live-1"],
		"staging/api must NOT be treated as a prod/api orphan (cross-namespace churn)")
	require.False(t, got["prod-live-1"], "the live in-store instance is not an orphan")
}

// TestGetServiceInstances_UnknownNamespaceNotReaped: a running container with no
// namespace label (empty Namespace) must be left alone rather than risk a
// cross-namespace false positive.
func TestGetServiceInstances_UnknownNamespaceNotReaped(t *testing.T) {
	ctx := context.Background()
	testStore := setupStore(t)
	testRunner := runner.NewTestRunner()
	rm := manager.NewTestRunnerManager(nil)
	rm.SetDockerRunner(testRunner)
	rm.SetProcessRunner(testRunner)
	ic := instancectl.NewController(testStore, rm, log.NewLogger())
	r := &Reconciler{store: testStore, instanceController: ic, logger: log.NewLogger()}

	svc := &types.Service{ID: "svc-prod-api", Name: "api", Namespace: "prod", Scale: 1}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeService, "prod", "api", svc))
	// Seed one prod instance so the instance namespace exists (TestStore.List
	// errors on a namespace with zero instances of the type).
	live := &types.Instance{ID: "prod-live-1", Namespace: "prod", ServiceName: "api", ServiceID: "svc-prod-api", Status: types.InstanceStatusRunning}
	require.NoError(t, testStore.Create(ctx, types.ResourceTypeInstance, "prod", live.ID, live))

	testRunner.Instances = map[string]*types.Instance{
		"prod-live-1": {ID: "prod-live-1", Namespace: "prod", ServiceName: "api", Status: types.InstanceStatusRunning},
		"no-ns-1":     {ID: "no-ns-1", Namespace: "", ServiceName: "api", Status: types.InstanceStatusRunning},
	}

	data, err := r.getServiceInstances(ctx, svc)
	require.NoError(t, err)
	require.Empty(t, data.OrphanedInstances, "an unlabeled-namespace container must not be reaped")
}
