package instance

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/runestack/rune/pkg/types"
)

// TestVolumeE2E_ClaimTemplateNStableOrdinals walks `resolveVolumeMount`
// across three ordinals (api-0, api-1, api-2) and asserts:
//   - three distinct Volume rows are auto-created with names data-api-N;
//   - each row carries the correct OwnerService + per-ordinal BoundClaim;
//   - re-resolving the same ordinals yields identical volume names
//     (per-ordinal binding is stable across reconciles).
func TestVolumeE2E_ClaimTemplateNStableOrdinals(t *testing.T) {
	ts, ic := newInstanceControllerForVolumeTests(t)

	svc := &types.Service{Name: "api", Namespace: "default"}
	mount := types.VolumeMount{
		Name:      "data",
		MountPath: "/data",
		ClaimTemplate: &types.VolumeClaimTemplate{
			StorageClassName: "fast",
			Size:             "1Gi",
			AccessMode:       types.AccessModeRWO,
			ReclaimPolicy:    types.ReclaimPolicyDelete,
		},
	}

	// First pass: resolver returns "not ready" for each ordinal but
	// stamps a Pending row each time.
	for ord := 0; ord < 3; ord++ {
		_, err := ic.resolveVolumeMount(context.Background(), svc,
			&types.Instance{Name: fmt.Sprintf("api-%d", ord), Ordinal: ord}, mount)
		require.Error(t, err, "first-pass resolver returns not-ready")
	}

	expectedNames := []string{"data-api-0", "data-api-1", "data-api-2"}
	for ord, want := range expectedNames {
		var v types.Volume
		require.NoError(t, ts.Get(context.Background(), types.ResourceTypeVolume,
			"default", want, &v), "volume %s must exist after first-pass resolve", want)
		assert.Equal(t, "default/api", v.OwnerService, "%s ownership", want)
		assert.Equal(t, fmt.Sprintf("api/data/%d", ord), v.BoundClaim, "%s binding", want)
	}

	// Pre-populate handles so a second pass succeeds.
	for ord, name := range expectedNames {
		var v types.Volume
		require.NoError(t, ts.Get(context.Background(), types.ResourceTypeVolume, "default", name, &v))
		v.Status = types.VolumeStatusAvailable
		v.Handle = fmt.Sprintf("/srv/rune/%s", name)
		require.NoError(t, ts.Update(context.Background(), types.ResourceTypeVolume, "default", name, &v))
		_ = ord
	}

	// Second pass: resolver succeeds and returns the same volume names
	// for each ordinal — proving the per-ordinal binding is stable.
	for ord, want := range expectedNames {
		got, err := ic.resolveVolumeMount(context.Background(), svc,
			&types.Instance{Name: fmt.Sprintf("api-%d", ord), Ordinal: ord}, mount)
		require.NoError(t, err)
		assert.Equal(t, want, got.VolumeName, "ord %d must always resolve to %s", ord, want)
		assert.Equal(t, fmt.Sprintf("/srv/rune/%s", want), got.Source, "ord %d source", ord)
	}

	// And no extra volumes leaked.
	var all []types.Volume
	require.NoError(t, ts.List(context.Background(), types.ResourceTypeVolume, "default", &all))
	assert.Len(t, all, 3, "exactly three per-ordinal volumes")
}
