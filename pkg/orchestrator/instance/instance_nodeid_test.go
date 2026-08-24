package instance

import (
	"testing"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Instances carry the node's real identity — the same string
// Volume.BoundNode and the observability stream label use — not the
// pre-RUNE-301 literal "local".
func TestCreateInstance_StampsWiredNodeID(t *testing.T) {
	ctx, testStore, _, _ := setupTestController(t)

	testRunner := runner.NewTestRunner()
	mgr := manager.NewTestRunnerManager(nil)
	mgr.SetDockerRunner(testRunner)
	mgr.SetProcessRunner(testRunner)
	controller := NewController(testStore, mgr, log.NewLogger(), WithNodeID("node-8f6a12cd"))

	svc := instanceControllerCreateTestService(ctx, t, testStore, "gpu-service", types.RestartPolicyAlways)
	inst, err := controller.CreateInstance(ctx, svc, "gpu-service-0", 0)
	require.NoError(t, err)
	assert.Equal(t, "node-8f6a12cd", inst.NodeID)

	stored, err := testStore.GetInstanceByID(ctx, "default", inst.ID)
	require.NoError(t, err)
	assert.Equal(t, "node-8f6a12cd", stored.NodeID)
}

// With no identity wired — embedded use and unit tests — the pre-301
// literal is preserved, because Instance.Validate() requires a non-empty
// nodeId and those callers have no node-identity.json to read.
func TestCreateInstance_FallsBackToLocalWithoutIdentity(t *testing.T) {
	ctx, testStore, _, controller := setupTestController(t)

	assert.Equal(t, types.LocalNodeIDFallback, controller.NodeID())

	svc := instanceControllerCreateTestService(ctx, t, testStore, "plain-service", types.RestartPolicyAlways)
	inst, err := controller.CreateInstance(ctx, svc, "plain-service-0", 0)
	require.NoError(t, err)
	assert.Equal(t, "local", inst.NodeID)
}
