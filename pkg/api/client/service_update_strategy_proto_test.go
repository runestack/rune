package client

// RUNE-042 Phase 1: updateStrategy, drainSeconds and the generation counters
// must survive the proto round-trip. A field that exists on the struct but
// not on the wire is silently dropped before it reaches the controller —
// exactly how imagePullAnonymous shipped broken in v0.0.1-dev.139.

import (
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServiceProto_UpdateStrategyRoundTrip(t *testing.T) {
	drain := 20
	svc := &types.Service{
		Name:           "api",
		Namespace:      "default",
		Image:          "app:v1",
		Scale:          3,
		Runtime:        types.RuntimeTypeContainer,
		UpdateStrategy: &types.UpdateStrategy{Type: types.UpdateRecreate},
		DrainSeconds:   &drain,
	}

	back, err := ProtoToService(ServiceToProto(svc))
	require.NoError(t, err)

	require.NotNil(t, back.UpdateStrategy, "updateStrategy must survive the wire")
	assert.Equal(t, types.UpdateRecreate, back.UpdateStrategy.Type)
	require.NotNil(t, back.DrainSeconds, "drainSeconds must survive the wire")
	assert.Equal(t, 20, *back.DrainSeconds)
}

// Omitted stays omitted: an unset strategy must not come back as an explicit
// one, or `rune get -o yaml` would start showing a field the operator never
// wrote and a re-cast would look like a change.
func TestServiceProto_UpdateStrategyOmittedStaysOmitted(t *testing.T) {
	svc := &types.Service{
		Name: "api", Namespace: "default", Image: "app:v1",
		Scale: 1, Runtime: types.RuntimeTypeContainer,
	}

	back, err := ProtoToService(ServiceToProto(svc))
	require.NoError(t, err)

	assert.Nil(t, back.UpdateStrategy, "unset strategy must stay unset (it means rolling)")
	assert.Nil(t, back.DrainSeconds, "unset drain must stay unset (it means the default)")

	// And the derived params are still correct on the far side.
	p := back.ResolveUpdateParams()
	assert.Equal(t, types.UpdateRolling, p.Type)
	assert.Equal(t, types.DefaultDrainSeconds*int(1), int(p.Drain.Seconds()))
}

// The generation counters close a known gap: observedGeneration was not on
// the wire at all, so clients and the dashboard could not see reconcile
// progress. All three must round-trip at full int64 width.
func TestServiceProto_GenerationCountersRoundTrip(t *testing.T) {
	svc := &types.Service{
		Name: "api", Namespace: "default", Image: "app:v1",
		Scale: 2, Runtime: types.RuntimeTypeContainer,
		Metadata: &types.ServiceMetadata{
			Generation:         42,
			TemplateGeneration: 40,
			ObservedGeneration: 41,
			LastNonZeroScale:   2,
		},
	}

	back, err := ProtoToService(ServiceToProto(svc))
	require.NoError(t, err)
	require.NotNil(t, back.Metadata)

	assert.Equal(t, int64(42), back.Metadata.Generation)
	assert.Equal(t, int64(40), back.Metadata.TemplateGeneration, "template generation must be visible to clients")
	assert.Equal(t, int64(41), back.Metadata.ObservedGeneration, "observed generation must be visible to clients")
	assert.Equal(t, 2, back.Metadata.LastNonZeroScale)
}
