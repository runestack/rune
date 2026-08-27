package types

// Environment variables that scope a container to its devices. Rune owns
// both: NVIDIA_VISIBLE_DEVICES is read by the container toolkit's prestart
// hook, CUDA_VISIBLE_DEVICES in-process at cuInit.
const (
	EnvNvidiaVisibleDevices = "NVIDIA_VISIBLE_DEVICES"
	EnvCudaVisibleDevices   = "CUDA_VISIBLE_DEVICES"

	// DevicesDenied is the toolkit's "do not run the hook at all" value.
	//
	// Set wherever a container must have no GPUs, rather than leaving the
	// variable unset. Unset is not denial: stock CUDA, vLLM and TEI images
	// ship NVIDIA_VISIBLE_DEVICES=all, so where the legacy nvidia runtime
	// is Docker's default, silence hands that container every card — with
	// no GPU request, no privilege, and nothing in the ledger.
	//
	// "void" not "none": none still runs the hook and injects the CUDA
	// libraries.
	DevicesDenied = "void"
)

// GPUVisibilityVars are those variables. One list: the spec validator
// rejects them alongside resources.gpu, the orchestrator writes them, and
// both runners strip them.
var GPUVisibilityVars = []string{EnvNvidiaVisibleDevices, EnvCudaVisibleDevices}

// IsGPUVisibilityVar reports whether key is one of them.
func IsGPUVisibilityVar(key string) bool {
	for _, k := range GPUVisibilityVars {
		if k == key {
			return true
		}
	}
	return false
}
