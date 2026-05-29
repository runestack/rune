package controllers

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// When ContainerIP is already cached on the instance, instanceEndpointIP
// must backfill the top-level Instance.IP (historically left empty) onto
// the persisted record without needing a runner inspect.
func TestInstanceEndpointIP_BackfillsTopLevelIP(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)

	inst := &types.Instance{
		ID:        "i-1",
		Name:      "api-0",
		Namespace: "prod",
		Status:    types.InstanceStatusRunning,
		Metadata:  &types.InstanceMetadata{ContainerIP: "10.96.0.46"},
	}
	require.NoError(t, ts.Create(context.Background(), types.ResourceTypeInstance, inst.Namespace, inst.ID, inst))

	got := ic.instanceEndpointIP(context.Background(), inst)
	assert.Equal(t, "10.96.0.46", got)

	// In-hand copy updated.
	assert.Equal(t, "10.96.0.46", inst.IP)

	// Persisted record now carries the top-level IP, not just ContainerIP.
	var fresh types.Instance
	require.NoError(t, ts.Get(context.Background(), types.ResourceTypeInstance, "prod", "i-1", &fresh))
	assert.Equal(t, "10.96.0.46", fresh.IP)
	assert.Equal(t, "10.96.0.46", fresh.Metadata.ContainerIP)
}

// No IP known anywhere → empty, no panic, no spurious write.
func TestInstanceEndpointIP_NoIP(t *testing.T) {
	_, ic := newInstanceControllerForVolumeTests(t)
	inst := &types.Instance{ID: "i-2", Name: "api-1", Namespace: "prod"}
	assert.Equal(t, "", ic.instanceEndpointIP(context.Background(), inst))
}
