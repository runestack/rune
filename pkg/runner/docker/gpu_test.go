package docker

import (
	"testing"

	"github.com/docker/docker/api/types/container"

	runetypes "github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gpuInstance(devices ...string) *runetypes.Instance {
	return &runetypes.Instance{
		ID: "i-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm", ServiceID: "svc-1",
		Metadata:       &runetypes.InstanceMetadata{Image: "vllm/vllm-openai"},
		GPUAssignments: devices,
		Environment: map[string]string{
			"NVIDIA_VISIBLE_DEVICES": "GPU-1",
			"CUDA_VISIBLE_DEVICES":   "GPU-1",
			"HF_TOKEN":               "secret",
		},
	}
}

// Count and DeviceIDs are an either/or pair and -1 is "every device on the
// host", so the pairing is the thing to pin, not just the ID list.
func TestApplyDeviceRequests_ScopesToTheAssignment(t *testing.T) {
	hc := &container.HostConfig{}
	applyDeviceRequests(hc, gpuInstance("GPU-1", "GPU-2"))

	require.Len(t, hc.Resources.DeviceRequests, 1)
	req := hc.Resources.DeviceRequests[0]
	assert.Equal(t, "nvidia", req.Driver)
	assert.Equal(t, 0, req.Count, "-1 would be every device on the host")
	assert.Equal(t, []string{"GPU-1", "GPU-2"}, req.DeviceIDs)
	assert.Equal(t, [][]string{{"gpu"}}, req.Capabilities)
}

// A service with no GPU request must produce a byte-identical host config.
func TestApplyDeviceRequests_NoAssignmentAsksForNothing(t *testing.T) {
	hc := &container.HostConfig{}
	applyDeviceRequests(hc, &runetypes.Instance{ID: "i-1"})
	assert.Nil(t, hc.Resources.DeviceRequests)

	applyDeviceRequests(hc, nil)
	assert.Nil(t, hc.Resources.DeviceRequests)
}

// The assignment must not alias the instance's slice: a later append on
// either side would otherwise reach through into the other.
func TestApplyDeviceRequests_CopiesTheAssignment(t *testing.T) {
	inst := gpuInstance("GPU-1")
	hc := &container.HostConfig{}
	applyDeviceRequests(hc, inst)

	inst.GPUAssignments[0] = "GPU-9"
	assert.Equal(t, []string{"GPU-1"}, hc.Resources.DeviceRequests[0].DeviceIDs)
}

// The design's rule is that init steps get no assigned devices, and it
// enforces that by keeping DeviceRequests out of this path. That is only
// half: an init step copies the parent's environment wholesale, and with
// the legacy nvidia runtime as Docker's default NVIDIA_VISIBLE_DEVICES
// grants the cards on its own — so a fetch step would hold the devices the
// engine is waiting for, without ever asking for one.
func TestInitStep_DoesNotInheritDeviceScoping(t *testing.T) {
	r := makeInitTestRunner()
	inst := gpuInstance("GPU-1")
	step := runetypes.InitStep{Name: "fetch", Image: "alpine", Command: "true"}

	cfg, hostCfg, err := r.initStepToContainerConfig(inst, step)
	require.NoError(t, err)

	assert.Nil(t, hostCfg.Resources.DeviceRequests, "a step asks for no devices")
	assert.Contains(t, cfg.Env, "NVIDIA_VISIBLE_DEVICES=void",
		"denied, not merely unmentioned: unset lets the step's own image decide")
	assert.Contains(t, cfg.Env, "CUDA_VISIBLE_DEVICES=")
	assert.NotContains(t, cfg.Env, "CUDA_VISIBLE_DEVICES=GPU-1")
	assert.Contains(t, cfg.Env, "HF_TOKEN=secret",
		"the rest of the parent environment still comes along")
}

// Through the real config builder, not the helper: a correct helper that
// nothing calls scopes nothing.
func TestInstanceToContainerConfig_CarriesTheDeviceRequest(t *testing.T) {
	r := makeInitTestRunner()

	_, hostCfg, err := r.instanceToContainerConfig(gpuInstance("GPU-1"))
	require.NoError(t, err)
	require.Len(t, hostCfg.Resources.DeviceRequests, 1)
	assert.Equal(t, []string{"GPU-1"}, hostCfg.Resources.DeviceRequests[0].DeviceIDs)

	// And a service that asked for nothing is unchanged.
	_, plainCfg, err := r.instanceToContainerConfig(&runetypes.Instance{
		ID: "i-2", Name: "web-0", Namespace: "default", ServiceName: "web", ServiceID: "svc-2",
		Metadata: &runetypes.InstanceMetadata{Image: "nginx"},
	})
	require.NoError(t, err)
	assert.Nil(t, plainCfg.Resources.DeviceRequests)
}

// A CPU-only service that pins CUDA_VISIBLE_DEVICES for its own reasons
// keeps it. Rune strips what Rune set; a value on an instance with no
// assignment is the user's.
func TestInitStep_KeepsAUserValueOnANonGPUService(t *testing.T) {
	r := makeInitTestRunner()
	inst := &runetypes.Instance{
		ID: "i-1", Name: "web-0", Namespace: "default", ServiceName: "web", ServiceID: "svc-1",
		Metadata: &runetypes.InstanceMetadata{Image: "alpine"},
		// A value the fill would NOT produce, so the assertion can tell
		// "kept the user's" from "stripped it and refilled with empty".
		Environment: map[string]string{"CUDA_VISIBLE_DEVICES": "0"},
	}
	step := runetypes.InitStep{Name: "fetch", Image: "alpine", Command: "true"}

	cfg, _, err := r.initStepToContainerConfig(inst, step)
	require.NoError(t, err)
	assert.Contains(t, cfg.Env, "CUDA_VISIBLE_DEVICES=0")
	assert.NotContains(t, cfg.Env, "CUDA_VISIBLE_DEVICES=")
}

// The postmortem sidecar takes no devices; see debugSidecarConfig.
func TestRunDebug_SidecarTakesNoDevices(t *testing.T) {
	r := makeInitTestRunner()
	inst := gpuInstance("GPU-1")

	_, workloadHostCfg, err := r.instanceToContainerConfig(inst)
	require.NoError(t, err)
	require.NotNil(t, workloadHostCfg.Resources.DeviceRequests,
		"the workload path does take them, or this test proves nothing")

	cfg, hostCfg, err := r.debugSidecarConfig(inst)
	require.NoError(t, err)

	assert.Nil(t, hostCfg.Resources.DeviceRequests)
	assert.Contains(t, cfg.Env, "NVIDIA_VISIBLE_DEVICES=void")
	assert.Contains(t, cfg.Env, "CUDA_VISIBLE_DEVICES=")
	assert.NotContains(t, cfg.Env, "NVIDIA_VISIBLE_DEVICES=GPU-1",
		"the workload's own value must not survive alongside the denial")
	assert.NotContains(t, cfg.Env, "CUDA_VISIBLE_DEVICES=GPU-1")
}

// The sidecar exists to show a failed container's own state, so a value
// the user set is theirs there too — the same rule the workload path
// follows. Only Rune's own assignment is dropped.
func TestRunDebug_SidecarKeepsAUserValueOnANonGPUService(t *testing.T) {
	r := makeInitTestRunner()
	inst := &runetypes.Instance{
		ID: "i-1", Name: "web-0", Namespace: "default", ServiceName: "web", ServiceID: "svc-1",
		Metadata:    &runetypes.InstanceMetadata{Image: "alpine"},
		Environment: map[string]string{"CUDA_VISIBLE_DEVICES": "all"},
	}

	cfg, hostCfg, err := r.debugSidecarConfig(inst)
	require.NoError(t, err)

	assert.Nil(t, hostCfg.Resources.DeviceRequests)
	assert.Contains(t, cfg.Env, "NVIDIA_VISIBLE_DEVICES=void")
	assert.Contains(t, cfg.Env, "CUDA_VISIBLE_DEVICES=all",
		"the workload path keeps this, so the postmortem beside it must too")
}

// The builder denies on its own, so the invariant is a property of the
// builder rather than of its callers having gone through the orchestrator.
func TestInstanceToContainerConfig_ScopesWhenTheEnvironmentWasNotResolved(t *testing.T) {
	r := makeInitTestRunner()

	// Instances carrying no resolved environment at all — what a record
	// rebuilt outside the orchestrator's env assembly looks like. Both
	// branches, because a scoped DeviceRequest still loses to an image's
	// built-in "all" if nothing says otherwise.
	base := func() *runetypes.Instance {
		return &runetypes.Instance{
			ID: "i-1", Name: "web-0", Namespace: "default", ServiceName: "web", ServiceID: "svc-1",
			Metadata: &runetypes.InstanceMetadata{Image: "nvidia/cuda:12.4.0-base"},
		}
	}

	cfg, hostCfg, err := r.instanceToContainerConfig(base())
	require.NoError(t, err)
	assert.Nil(t, hostCfg.Resources.DeviceRequests)
	assert.Contains(t, cfg.Env, "NVIDIA_VISIBLE_DEVICES=void",
		"an unresolved environment must not leave the image's own value to decide")

	assigned := base()
	assigned.GPUAssignments = []string{"GPU-1"}
	cfg, hostCfg, err = r.instanceToContainerConfig(assigned)
	require.NoError(t, err)
	require.Len(t, hostCfg.Resources.DeviceRequests, 1)
	assert.Contains(t, cfg.Env, "NVIDIA_VISIBLE_DEVICES=GPU-1",
		"the device request alone loses to the image under the legacy runtime")
	assert.Contains(t, cfg.Env, "CUDA_VISIBLE_DEVICES=GPU-1")
}

// Both branches return a copy. Returning the record's own map on the
// assigned branch would mean a consumer that ever mutates the result
// corrupts the instance — on GPU instances only, so it could not
// reproduce on a box without cards.
func TestDeviceScopedEnv_NeverAliasesTheInstanceRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		assign []string
	}{
		{"assigned", []string{"GPU-1"}},
		{"unassigned", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &runetypes.Instance{
				ID: "i-1", GPUAssignments: tc.assign,
				Environment: map[string]string{"HF_TOKEN": "secret"},
			}

			got := deviceScopedEnv(inst)
			got["HF_TOKEN"] = "clobbered"

			assert.Equal(t, "secret", inst.Environment["HF_TOKEN"])
		})
	}
}

// The third member of the family: the main container's own builder honours
// a user value too. Without this, flipping its literal false would leave
// the main container disagreeing with the init step and the sidecar beside
// it, which is the asymmetry the shared helper exists to prevent.
func TestInstanceToContainerConfig_KeepsAUserValueOnANonGPUService(t *testing.T) {
	r := makeInitTestRunner()

	cfg, _, err := r.instanceToContainerConfig(&runetypes.Instance{
		ID: "i-1", Name: "web-0", Namespace: "default", ServiceName: "web", ServiceID: "svc-1",
		Metadata:    &runetypes.InstanceMetadata{Image: "alpine"},
		Environment: map[string]string{"CUDA_VISIBLE_DEVICES": "0"},
	})
	require.NoError(t, err)

	assert.Contains(t, cfg.Env, "CUDA_VISIBLE_DEVICES=0")
	assert.NotContains(t, cfg.Env, "CUDA_VISIBLE_DEVICES=")
}
