package process

import (
	"testing"

	runetypes "github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

// Same rule as the docker path, for the same reason: a step is not the
// workload the devices were assigned to. Here there is no hook to strip
// them for us, so the parent's CUDA_VISIBLE_DEVICES would simply be read
// by whatever the step runs.
func TestInitStep_DoesNotInheritDeviceScoping(t *testing.T) {
	inst := &runetypes.Instance{
		ID: "i-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		GPUAssignments: []string{"GPU-1"},
		Environment: map[string]string{
			"NVIDIA_VISIBLE_DEVICES": "GPU-1",
			"CUDA_VISIBLE_DEVICES":   "GPU-1",
			"HF_TOKEN":               "secret",
		},
	}
	step := runetypes.InitStep{Name: "fetch", Command: "true"}

	env := initStepEnv(inst, step)

	// This runner has no hook, so NVIDIA_VISIBLE_DEVICES is inert here and
	// the denial that matters is the empty CUDA_VISIBLE_DEVICES. Absent
	// would mean every device.
	assert.Contains(t, env, "CUDA_VISIBLE_DEVICES=")
	assert.NotContains(t, env, "CUDA_VISIBLE_DEVICES=GPU-1")
	assert.Contains(t, env, "NVIDIA_VISIBLE_DEVICES=void")
	assert.Contains(t, env, "HF_TOKEN=secret")
}

// runed's own environment is inherited so a step can find PATH, which
// means it can carry a device scoping too. An operator who exported
// CUDA_VISIBLE_DEVICES in the unit file must not hand it to every step.
func TestInitStepEnv_DropsTheRunnersOwnScoping(t *testing.T) {
	t.Setenv("CUDA_VISIBLE_DEVICES", "all")
	t.Setenv("RUNE_TEST_MARKER", "kept")

	env := initStepEnv(&runetypes.Instance{}, runetypes.InitStep{Name: "fetch"})

	assert.NotContains(t, env, "CUDA_VISIBLE_DEVICES=all")
	assert.Contains(t, env, "CUDA_VISIBLE_DEVICES=", "denied, not inherited")
	assert.Contains(t, env, "RUNE_TEST_MARKER=kept", "the rest of it is still inherited")
}

// The process runner is the one where CUDA_VISIBLE_DEVICES is all there
// is, so a user value there matters more, not less. Same rule as the
// container path: Rune strips what Rune set, and does not rewrite the
// rest.
func TestInitStepEnv_KeepsAUserValueOnANonGPUService(t *testing.T) {
	inst := &runetypes.Instance{
		ID: "i-1", Name: "web-0", Namespace: "default", ServiceName: "web",
		Environment: map[string]string{"CUDA_VISIBLE_DEVICES": "0"},
	}

	env := initStepEnv(inst, runetypes.InitStep{Name: "fetch"})

	assert.Contains(t, env, "CUDA_VISIBLE_DEVICES=0")
	assert.NotContains(t, env, "CUDA_VISIBLE_DEVICES=",
		"appending unconditionally would beat the user's value, since exec takes the last")
}
