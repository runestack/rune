package instance

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

// A create-time failure is wrapped by runCreateAttempt before it reaches
// the classifier, so classifying a bare error proves nothing — the generic
// "failed to create instance:" case sits on the same string. Both wrapper
// shapes, because which one Docker uses is a daemon-version detail.
func TestClassifyCreateError_NamesGPUHostFailuresThroughBothWrappers(t *testing.T) {
	cases := []struct {
		name, want string
		inner      string
	}{
		{"no nvidia runtime", types.GPUReasonToolkitMissing,
			`Error response from daemon: could not select device driver "nvidia" with capabilities: [[gpu]]`},
		{"toolkit hook absent", types.GPUReasonToolkitMissing,
			"exec: \"nvidia-container-cli\": executable file not found in $PATH"},
		// A card that went away is NOT a missing toolkit: one needs the
		// node re-probed, the other needs the toolkit installed.
		// One case per phrase: a single string carrying both pins neither.
		{"assigned card is gone", types.GPUReasonDeviceMissing,
			"nvidia-container-cli: device error: GPU-1: unknown device"},
		{"hook reports only a device error", types.GPUReasonDeviceMissing,
			"nvidia-container-cli: initialization error: device error"},
		{"hook reports only an unknown device", types.GPUReasonDeviceMissing,
			"nvidia-container-cli: unknown device GPU-9"},
	}
	for _, tc := range cases {
		for _, wrap := range []string{"failed to create instance: %w", "failed to start instance: %w"} {
			t.Run(tc.name+" / "+strings.Fields(wrap)[1], func(t *testing.T) {
				assert.Equal(t, tc.want, classifyCreateError(fmt.Errorf(wrap, errors.New(tc.inner))))
			})
		}
	}

	// Ordinary failures keep their own slugs through the same wrappers.
	assert.Equal(t, "RunnerCreateError",
		classifyCreateError(fmt.Errorf("failed to create instance: %w", errors.New("port is already allocated"))))
	assert.Equal(t, "RunnerStartError",
		classifyCreateError(fmt.Errorf("failed to start instance: %w", errors.New("no such file or directory"))))
}

// A spec naming either key must not make the instance permanently
// outdated — that is a service replacing itself forever.
func TestClassifyObserved_DeviceScopingDoesNotMakeAServicePermanentlyOutdated(t *testing.T) {
	for _, key := range []string{"NVIDIA_VISIBLE_DEVICES", "CUDA_VISIBLE_DEVICES"} {
		t.Run(key, func(t *testing.T) {
			svc := &types.Service{
				Name: "vllm", Namespace: "default", Runtime: "docker",
				Env: map[string]string{key: "all"},
			}
			inst := &types.Instance{
				ID: "i-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
				ContainerID: "c-1", Status: types.InstanceStatusRunning,
				Environment: map[string]string{
					"NVIDIA_VISIBLE_DEVICES": "void",
					"CUDA_VISIBLE_DEVICES":   "",
				},
			}

			verdict := classifyObserved(inst, svc,
				instanceObservation{status: types.InstanceStatusRunning}, log.NewTestLogger())
			assert.NotEqual(t, CompatOutdated, verdict.Class,
				"%s makes every pass see a mismatch Rune itself wrote", key)
		})
	}
}

// Both device phrases are ordinary English. Without the hook's name
// qualifying them, an unrelated failure on a box with no GPUs at all
// reports a missing card.
func TestClassifyCreateError_DeviceWordsAloneAreNotAGPUFailure(t *testing.T) {
	for _, inner := range []string{
		"error gathering device information: unknown device",
		"failed to mount volume: device error",
	} {
		got := classifyCreateError(fmt.Errorf("failed to create instance: %w", errors.New(inner)))
		assert.Equal(t, "RunnerCreateError", got, "inner: %s", inner)
	}
}
