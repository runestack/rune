package client

import (
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gpuProtoService(g *types.GPURequest) *types.Service {
	return &types.Service{
		ID: "s-1", Name: "vllm", Namespace: "default",
		Resources: types.Resources{GPU: g},
	}
}

// Both directions in one test, because half a mapping is worse than none:
// with only one side, the stored spec carries a request the rendered one
// does not, so every cast of that service compares unequal forever and
// strips the request again each time.
func TestGPURequest_SurvivesTheProtoRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		gpu  *types.GPURequest
	}{
		{"whole device, implicit", &types.GPURequest{}},
		{"whole device, explicit", &types.GPURequest{Count: 1}},
		{"several devices", &types.GPURequest{Count: 4}},
		{"a share", &types.GPURequest{VRAM: "20Gi"}},
		{"heterogeneous", &types.GPURequest{Count: 2, AllowHeterogeneous: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			back, err := ProtoToService(ServiceToProto(gpuProtoService(tc.gpu)))
			require.NoError(t, err)
			require.NotNil(t, back.Resources.GPU, "the request was dropped in flight")
			assert.Equal(t, *tc.gpu, *back.Resources.GPU)
		})
	}
}

// Absent must stay absent. A nil request and an empty one mean opposite
// things — no GPU at all, versus one whole device — so a round trip that
// turned nil into {} would silently claim a card.
func TestGPURequest_AbsentStaysAbsentAcrossTheRoundTrip(t *testing.T) {
	back, err := ProtoToService(ServiceToProto(gpuProtoService(nil)))
	require.NoError(t, err)
	assert.Nil(t, back.Resources.GPU)
}

// A service whose only resource is a GPU still emits a Resources message —
// the outbound guard is a struct comparison, and a pointer field has to
// make it non-zero or the request never leaves.
func TestGPURequest_GPUOnlyServiceStillEmitsResources(t *testing.T) {
	p := ServiceToProto(gpuProtoService(&types.GPURequest{Count: 1}))
	require.NotNil(t, p.Resources, "a GPU-only request must not be dropped by the zero check")
	require.NotNil(t, p.Resources.Gpu)
}
