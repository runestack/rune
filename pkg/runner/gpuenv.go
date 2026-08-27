package runner

import "github.com/runestack/rune/pkg/types"

// The device-scoping variables and their denial value live in pkg/types,
// with the spec validator that rejects them from a GPU service's env.
const (
	EnvNvidiaVisibleDevices = types.EnvNvidiaVisibleDevices
	EnvCudaVisibleDevices   = types.EnvCudaVisibleDevices
	DevicesDenied           = types.DevicesDenied
)

// IsGPUVisibilityVar reports whether key is one Rune owns.
func IsGPUVisibilityVar(key string) bool { return types.IsGPUVisibilityVar(key) }

// DenyDevices marks an environment as entitled to no GPUs.
//
// Both keys, because the runners read different ones: under Docker the
// hook reads NVIDIA_VISIBLE_DEVICES; a bare process has no hook and reads
// CUDA_VISIBLE_DEVICES, where ABSENT means every device — so denial there
// is an explicit empty value, not a removal.
//
// CUDA_VISIBLE_DEVICES is filled, not overwritten: a value already here is
// the user's. That holds on all three paths in, for three different
// reasons — the resolved environment is rebuilt from the spec each time;
// DeniedEnv strips Rune's scoping first when the instance held an
// assignment; and when it held none there was never anything to strip,
// because GPUAssignments is written once and never cleared.
//
// That last one is an invariant nothing enforces. Clearing GPUAssignments
// on release is a plausible cleanup, and the day it lands this starts
// preserving a pin naming a card someone else now holds — inert under
// Docker with the hook off, live on a bare process.
func DenyDevices(env map[string]string) {
	env[EnvNvidiaVisibleDevices] = DevicesDenied
	if _, ok := env[EnvCudaVisibleDevices]; !ok {
		env[EnvCudaVisibleDevices] = ""
	}
}

// DeniedEnv is a copy of an instance's environment entitled to no GPUs.
//
// hadAssignment decides which existing values to drop first: when Rune
// granted this instance devices, the scoping in env names cards this
// container must not have. When it granted nothing, what is there is the
// user's and stays.
func DeniedEnv(env map[string]string, hadAssignment bool) map[string]string {
	out := make(map[string]string, len(env)+2)
	for k, v := range env {
		if hadAssignment && IsGPUVisibilityVar(k) {
			continue
		}
		out[k] = v
	}
	DenyDevices(out)
	return out
}
