package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func surgeService(g *GPURequest) *Service {
	return &Service{
		Name: "vllm", Namespace: "default", Runtime: "docker",
		Scale: 1, Resources: Resources{GPU: g},
	}
}

// A whole-device claim cannot surge: the replacement would be admitted
// against the card the outgoing instance still holds, and admission
// refuses before an instance record exists — so nothing carries the
// reason, the rollout retries until it stalls, and neither rune restart
// nor reverting the spec clears it.
func TestIsSurgeCapable_WholeDeviceGPUCannotSurge(t *testing.T) {
	assert.False(t, surgeService(&GPURequest{}).IsSurgeCapable(),
		"an implicit whole device is still a whole device")
	assert.False(t, surgeService(&GPURequest{Count: 2}).IsSurgeCapable())
	assert.Equal(t, "a whole-device GPU claim", surgeBlocker(surgeService(&GPURequest{})))
}

// A share can surge if the card has room. Whether it does is admission's
// call, not the planner's — refusing here would make every shared GPU
// service dip on a box with plenty of VRAM free.
func TestIsSurgeCapable_SharedDeviceStillSurges(t *testing.T) {
	assert.True(t, surgeService(&GPURequest{VRAM: "20Gi"}).IsSurgeCapable())
}

// And a service with no GPU is untouched.
func TestIsSurgeCapable_NoGPUIsUnchanged(t *testing.T) {
	assert.True(t, surgeService(nil).IsSurgeCapable())
}
