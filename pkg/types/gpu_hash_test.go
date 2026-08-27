package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func gpuHashService(g *GPURequest) *Service {
	return &Service{
		Name: "vllm", Namespace: "default", Image: "vllm/vllm-openai", Scale: 1,
		Resources: Resources{GPU: g},
	}
}

func TestTemplateHash_ChangesWithTheDeviceRequest(t *testing.T) {
	base := gpuHashService(&GPURequest{Count: 1}).CalculateTemplateHash()

	for _, tc := range []struct {
		name string
		gpu  *GPURequest
	}{
		{"more devices", &GPURequest{Count: 2}},
		{"a vram share instead", &GPURequest{VRAM: "20Gi"}},
		{"heterogeneous allowed", &GPURequest{Count: 1, AllowHeterogeneous: true}},
		{"request removed", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotEqual(t, base, gpuHashService(tc.gpu).CalculateTemplateHash())
		})
	}
}

func TestTemplateHash_AbsentIsNotAnEmptyRequest(t *testing.T) {
	assert.NotEqual(t,
		gpuHashService(nil).CalculateTemplateHash(),
		gpuHashService(&GPURequest{}).CalculateTemplateHash())
}

// The full hash drives Generation, and a device edit is a real change.
func TestFullHash_ChangesWithTheDeviceRequest(t *testing.T) {
	assert.NotEqual(t,
		gpuHashService(&GPURequest{Count: 1}).CalculateHash(),
		gpuHashService(&GPURequest{Count: 2}).CalculateHash())
}

// A same-shape resize is the likeliest GPU edit there is, and the digest
// has to move for it.
func TestTemplateHash_ChangesWithTheVRAMShare(t *testing.T) {
	assert.NotEqual(t,
		gpuHashService(&GPURequest{VRAM: "20Gi"}).CalculateTemplateHash(),
		gpuHashService(&GPURequest{VRAM: "40Gi"}).CalculateTemplateHash())
}

// Zero means one, so spelling out the implicit count is a documentation
// edit — it must not reload a model.
func TestTemplateHash_ImplicitAndExplicitOneAgree(t *testing.T) {
	assert.Equal(t,
		gpuHashService(&GPURequest{}).CalculateTemplateHash(),
		gpuHashService(&GPURequest{Count: 1}).CalculateTemplateHash())
}
