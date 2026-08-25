package types

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func castOne(t *testing.T, body string) (*ServiceSpec, error) {
	t.Helper()
	cf, err := ParseCastFileFromBytes([]byte(body), "")
	require.NoError(t, err, "cast file must parse")
	require.Len(t, cf.Services, 1)
	spec := &cf.Services[0]
	return spec, spec.Validate()
}

const gpuSpecPrefix = `
service:
  name: vllm
  image: vllm/vllm-openai
  resources:
`

// A bare `gpu: {}` is the shape most users need first: one whole device,
// exclusively theirs, without knowing the model's VRAM footprint.
func TestGPUSpec_BareBlockMeansOneWholeDevice(t *testing.T) {
	spec, err := castOne(t, gpuSpecPrefix+"    gpu: {}\n")
	require.NoError(t, err)
	require.NotNil(t, spec.Resources.GPU)
	assert.Equal(t, 1, spec.Resources.GPU.DeviceCount())
	assert.False(t, spec.Resources.GPU.SharesDevice())
}

func TestGPUSpec_VRAMSharesADevice(t *testing.T) {
	spec, err := castOne(t, gpuSpecPrefix+"    gpu:\n      vram: \"20Gi\"\n")
	require.NoError(t, err)
	require.NotNil(t, spec.Resources.GPU)
	assert.True(t, spec.Resources.GPU.SharesDevice())
	assert.Equal(t, 1, spec.Resources.GPU.DeviceCount(), "a share still occupies one device")
}

// Absent must stay distinguishable from present-and-empty: a GPU-less
// service carries a nil request, not a zero-valued one.
func TestGPUSpec_AbsentIsNil(t *testing.T) {
	spec, err := castOne(t, gpuSpecPrefix+"    cpu: { request: \"1\" }\n")
	require.NoError(t, err)
	assert.Nil(t, spec.Resources.GPU)
	assert.Equal(t, 0, spec.Resources.GPU.DeviceCount())
	assert.False(t, spec.Resources.GPU.SharesDevice())
}

func TestGPUSpec_ValidationRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"negative count", "    gpu:\n      count: -1\n", "cannot be negative"},
		{"unparseable vram", "    gpu:\n      vram: \"20 gigs\"\n", "invalid resources.gpu.vram"},
		{"zero vram", "    gpu:\n      vram: \"0\"\n", "greater than zero"},
		{"count>1 with vram", "    gpu:\n      count: 2\n      vram: \"20Gi\"\n", "whole devices"},
		{"heterogeneous without count", "    gpu:\n      allowHeterogeneous: true\n", "meaningful only with count > 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := castOne(t, gpuSpecPrefix+tt.body)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// A GPU request is a statement about accounting; these are statements
// that accounting does not apply. privileged bind-mounts host /dev and
// grants the device cgroup c *:* rwm, so the container sees every card on
// the box whatever it reserved.
func TestGPUSpec_RejectsPrivilegeEscapes(t *testing.T) {
	const body = `
service:
  name: vllm
  image: vllm/vllm-openai
  securityContext:
%s
  resources:
    gpu:
      vram: "20Gi"
`
	tests := []struct {
		name string
		sc   string
		want string
	}{
		{"privileged", "    privileged: true", "securityContext.privileged"},
		{"capAdd", "    capAdd: [\"SYS_ADMIN\"]", "securityContext.capAdd"},
		{"seccomp unconfined", "    seccompProfile:\n      type: unconfined", "seccompProfile"},
		// The PascalCase spelling users copy-paste from k8s docs.
		{"seccomp Unconfined", "    seccompProfile:\n      type: Unconfined", "seccompProfile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := castOne(t, strings.Replace(body, "%s", tt.sc, 1))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// Without a GPU request these are unchanged — the escape hatch stays open
// for anyone who needs privileged and is not asking Rune to account for a
// device.
func TestGPUSpec_PrivilegedWithoutGPUIsUnchanged(t *testing.T) {
	const body = `
service:
  name: plain
  image: nginx
  securityContext:
    privileged: true
    capAdd: ["SYS_ADMIN"]
`
	_, err := castOne(t, body)
	require.NoError(t, err, "privileged without resources.gpu must still be accepted")
}

// Rune sets these from the device assignment. NVIDIA_VISIBLE_DEVICES=all
// in particular hands the container every card on the host.
func TestGPUSpec_RejectsCUDAEnvOverrides(t *testing.T) {
	for _, k := range []string{"CUDA_VISIBLE_DEVICES", "NVIDIA_VISIBLE_DEVICES"} {
		t.Run(k, func(t *testing.T) {
			body := "\nservice:\n  name: vllm\n  image: v\n  env:\n    " + k +
				": \"all\"\n  resources:\n    gpu: {}\n"
			_, err := castOne(t, body)
			require.Error(t, err)
			assert.Contains(t, err.Error(), k)
		})
	}
}

// The typo hole this closes is not "silently ignored" — it is worse.
// `vrm` decodes to a non-nil GPURequest with an empty VRAM, which means
// one WHOLE DEVICE: the typo converts a 20Gi share into an exclusive
// claim on the card.
func TestGPUSpec_UnknownSubKeyIsACastError(t *testing.T) {
	_, err := castOne(t, gpuSpecPrefix+"    gpu:\n      vrm: \"20Gi\"\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field 'vrm' in service.resources.gpu")
}

func TestGPUSpec_UnknownResourcesKeyIsACastError(t *testing.T) {
	_, err := castOne(t, gpuSpecPrefix+"    gpuu: {}\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field 'gpuu' in service.resources")
}

// vendor is reserved, not a typo, and the error should say which.
func TestGPUSpec_VendorIsReservedNotUnknown(t *testing.T) {
	_, err := castOne(t, gpuSpecPrefix+"    gpu:\n      vendor: nvidia\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")
	assert.NotContains(t, err.Error(), "unknown field 'vendor'")
}

// The allowlists are hand-maintained mirrors of the struct, which has
// shipped a real bug before (imagePullAnonymous). Guard the drift.
func TestValidGPUFieldsMatchesStruct(t *testing.T) {
	for _, name := range yamlTagNames(t, reflect.TypeOf(GPURequest{})) {
		assert.True(t, validGPUFields[name],
			"GPURequest has yaml field %q that validGPUFields does not accept — "+
				"every cast using it would be rejected as unknown", name)
	}
	for _, name := range yamlTagNames(t, reflect.TypeOf(Resources{})) {
		assert.True(t, validResourcesFields[name],
			"Resources has yaml field %q that validResourcesFields does not accept", name)
	}
}
