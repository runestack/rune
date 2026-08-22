package instance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// TestPersistHealedContainerMapping: the runner-side heal only mutates the
// reconcile pass's in-hand copy; this write-back is what stops the health
// controller's next probe from dialing the dead container's IP.
func TestPersistHealedContainerMapping(t *testing.T) {
	ctx := context.Background()
	testStore := store.NewTestStore()
	testRunnerMgr := manager.NewTestRunnerManager(nil)
	ic := NewController(testStore, testRunnerMgr, log.NewLogger())

	stored := &types.Instance{
		ID:          "healed-instance",
		Name:        "web-0",
		Namespace:   "default",
		ContainerID: "stale-container-id",
		IP:          "172.17.0.3",
		Metadata:    &types.InstanceMetadata{ContainerIP: "172.17.0.3"},
	}
	require.NoError(t, testStore.CreateInstance(ctx, stored))

	healed := *stored
	healed.ContainerID = "live-container-id"
	healed.Metadata = &types.InstanceMetadata{ContainerIP: "172.17.0.9"}

	ic.persistHealedContainerMapping(ctx, &healed)

	var got types.Instance
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", stored.ID, &got))
	require.Equal(t, "live-container-id", got.ContainerID)
	require.Equal(t, "172.17.0.9", got.Metadata.ContainerIP)
	require.Equal(t, "172.17.0.9", got.IP)

	// Idempotent: a second pass (another reconcile healing the same
	// record) is a silent skip, not an error or a duplicate write.
	ic.persistHealedContainerMapping(ctx, &healed)
	require.NoError(t, testStore.Get(ctx, types.ResourceTypeInstance, "default", stored.ID, &got))
	require.Equal(t, "live-container-id", got.ContainerID)
}
