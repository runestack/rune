package controllers

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/require"
)

// setupTestController creates a controller with test dependencies
func setupTestController(t *testing.T) (context.Context, *store.TestStore, *runner.TestRunner, *InstanceController) {
	ctx := context.Background()
	// Configure test store with reasonable defaults to support secret/config repos
	opts := store.StoreOptions{
		SecretEncryptionEnabled: true,
		KEKBytes:                []byte("0123456789abcdef0123456789abcdef"), // 32 bytes
		SecretLimits: store.Limits{
			MaxObjectBytes:   1 << 20, // 1MiB
			MaxKeyNameLength: 256,
		},
		ConfigLimits: store.Limits{
			MaxObjectBytes:   1 << 20,
			MaxKeyNameLength: 256,
		},
	}
	testStore := store.NewTestStoreWithOptions(opts)
	testRunner := runner.NewTestRunner()
	testRunnerMgr := manager.NewTestRunnerManager(nil)
	testRunnerMgr.SetDockerRunner(testRunner)
	testRunnerMgr.SetProcessRunner(testRunner)
	testLogger := log.NewLogger()

	controller := NewInstanceController(testStore, testRunnerMgr, testLogger)
	return ctx, testStore, testRunner, controller
}

// createTestService creates a test service in the store
func instanceControllerCreateTestService(ctx context.Context, t *testing.T, testStore *store.TestStore, name string, restartPolicy types.RestartPolicy) *types.Service {
	service := &types.Service{
		ID:            name,
		Name:          name,
		Namespace:     "default",
		RestartPolicy: restartPolicy,
		Image:         "test-image:latest",
		Command:       "test-command",
		Args:          []string{"arg1", "arg2"},
		Runtime:       "container",
		Env: map[string]string{
			"ENV_VAR1": "value1",
			"ENV_VAR2": "value2",
		},
		Metadata: &types.ServiceMetadata{
			Generation: 1,
		},
	}

	err := testStore.CreateService(ctx, service)
	require.NoError(t, err, "Failed to create test service")
	return service
}

// controllerTestStore is a tiny adapter to get the underlying TestStore
// back from a controller in tests that only carry the controller and
// need the store too. It avoids changing setup helpers used by every
// other test in this file.
func controllerTestStore(c *InstanceController) *store.TestStore {
	if ts, ok := c.store.(*store.TestStore); ok {
		return ts
	}
	return nil
}
