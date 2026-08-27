package instance

import (
	"context"

	"testing"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner/manager"
	"github.com/runestack/rune/pkg/store"
	"github.com/stretchr/testify/require"

	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

// Both variables, always. They are read by different layers and neither
// is sufficient alone: the toolkit hook consumes NVIDIA_VISIBLE_DEVICES
// when it decides which device nodes to expose, and CUDA_VISIBLE_DEVICES
// is read in-process at cuInit, which is all a bare process has.
func TestApplyGPUVisibility_SetsBothFromTheAssignment(t *testing.T) {
	env := map[string]string{}
	applyGPUVisibility(env, &types.Instance{GPUAssignments: []string{"GPU-1", "GPU-2"}})

	assert.Equal(t, "GPU-1,GPU-2", env["NVIDIA_VISIBLE_DEVICES"])
	assert.Equal(t, "GPU-1,GPU-2", env["CUDA_VISIBLE_DEVICES"])
}

// Stock CUDA, vLLM and TEI images ship NVIDIA_VISIBLE_DEVICES=all. Under
// the legacy nvidia runtime that value alone grants every card on the box,
// so inheriting it rather than overriding it makes the ledger decorative.
func TestApplyGPUVisibility_OverridesAnImagesAll(t *testing.T) {
	env := map[string]string{"NVIDIA_VISIBLE_DEVICES": "all"}
	applyGPUVisibility(env, &types.Instance{GPUAssignments: []string{"GPU-1"}})

	assert.Equal(t, "GPU-1", env["NVIDIA_VISIBLE_DEVICES"])
}

// A service that asked for no GPU must be DENIED one, not merely left
// unmentioned. Silence lets a stock CUDA image's baked-in
// NVIDIA_VISIBLE_DEVICES=all stand, which is a card taken with nothing in
// the ledger — and the engine that OOMs next is the one Rune blames.
func TestApplyGPUVisibility_NoAssignmentDeniesDevices(t *testing.T) {
	env := map[string]string{"HF_TOKEN": "secret", "NVIDIA_VISIBLE_DEVICES": "all"}
	applyGPUVisibility(env, &types.Instance{})

	assert.Equal(t, "void", env["NVIDIA_VISIBLE_DEVICES"])
	// Explicitly empty, not absent: on runner: process nothing reads
	// NVIDIA_VISIBLE_DEVICES, and an absent CUDA_VISIBLE_DEVICES means
	// every device.
	v, ok := env["CUDA_VISIBLE_DEVICES"]
	assert.True(t, ok, "denial has to name the variable a bare process reads")
	assert.Equal(t, "", v)
	assert.Equal(t, "secret", env["HF_TOKEN"])
}

// The list the orchestrator writes and the list the init paths strip are
// one list. Two would drift, and the drift would be a build step silently
// holding a card.
func TestGPUVisibilityVars_AreTheOnesTheRunnersStrip(t *testing.T) {
	// The NAMES, not just that the predicate agrees with the list — a
	// list that grew LD_LIBRARY_PATH would pass a self-consistency check
	// while stripping it from every init step and setting it to a device
	// UUID on every GPU instance.
	assert.Equal(t,
		[]string{"NVIDIA_VISIBLE_DEVICES", "CUDA_VISIBLE_DEVICES"},
		types.GPUVisibilityVars)
	assert.False(t, runner.IsGPUVisibilityVar("LD_LIBRARY_PATH"))
	assert.False(t, runner.IsGPUVisibilityVar("HF_TOKEN"))
}

// Through prepareEnvVars, not the helper: a correct helper nobody calls
// scopes nothing, and on runner: process this is the ONLY half there is.
func TestPrepareEnvVars_CarriesTheDeviceScoping(t *testing.T) {
	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	c := NewController(st, manager.NewTestRunnerManager(nil), log.NewTestLogger())
	svc := &types.Service{ID: "s-1", Name: "vllm", Namespace: "default"}

	assigned, _, err := c.prepareEnvVars(ctx, svc, &types.Instance{
		ID: "i-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		GPUAssignments: []string{"GPU-1"},
	})
	require.NoError(t, err)
	assert.Equal(t, "GPU-1", assigned["NVIDIA_VISIBLE_DEVICES"])
	assert.Equal(t, "GPU-1", assigned["CUDA_VISIBLE_DEVICES"])

	plain, _, err := c.prepareEnvVars(ctx, svc, &types.Instance{
		ID: "i-2", Name: "web-0", Namespace: "default", ServiceName: "web",
	})
	require.NoError(t, err)
	assert.Equal(t, "void", plain["NVIDIA_VISIBLE_DEVICES"])
	assert.Equal(t, "", plain["CUDA_VISIBLE_DEVICES"])
}

// The scoping is the LAST writer, which is what makes it enforcement
// rather than a default: a value from the spec, from envFrom, or baked
// into the image is overwritten, not merged with.
func TestPrepareEnvVars_ScopingBeatsASpecValue(t *testing.T) {
	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	c := NewController(st, manager.NewTestRunnerManager(nil), log.NewTestLogger())
	svc := &types.Service{
		ID: "s-1", Name: "vllm", Namespace: "default",
		Env: map[string]string{"NVIDIA_VISIBLE_DEVICES": "all"},
	}

	env, _, err := c.prepareEnvVars(ctx, svc, &types.Instance{
		ID: "i-1", Name: "vllm-0", Namespace: "default", ServiceName: "vllm",
		GPUAssignments: []string{"GPU-1"},
	})
	require.NoError(t, err)
	assert.Equal(t, "GPU-1", env["NVIDIA_VISIBLE_DEVICES"],
		"nothing server-side rejects this key, so the overwrite is what holds")
}

// A value the user pinned survives denial. Rune removes only what Rune
// set, even when the user's value is broader.
func TestApplyGPUVisibility_DenialDoesNotOverrideAUserValue(t *testing.T) {
	env := map[string]string{"CUDA_VISIBLE_DEVICES": "0"}
	applyGPUVisibility(env, &types.Instance{})

	assert.Equal(t, "void", env["NVIDIA_VISIBLE_DEVICES"])
	assert.Equal(t, "0", env["CUDA_VISIBLE_DEVICES"])
}

// The displaced value is unrecoverable from the returned map — by then it
// holds the denial — so the return value is the only thing that can carry
// it to a caller.
func TestPrepareEnvVars_ReportsWhatTheDenialDisplaced(t *testing.T) {
	st := store.NewBadgerStore(log.NewTestLogger())
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	c := NewController(st, manager.NewTestRunnerManager(nil), log.NewTestLogger())
	inst := &types.Instance{ID: "i-1", Name: "web-0", Namespace: "default", ServiceName: "web"}

	// A service asking for devices while declaring no resources.gpu.
	_, displaced, err := c.prepareEnvVars(ctx, &types.Service{
		ID: "s-1", Name: "web", Namespace: "default",
		Env: map[string]string{"NVIDIA_VISIBLE_DEVICES": "all"},
	}, inst)
	require.NoError(t, err)
	assert.Equal(t, "all", displaced,
		"the caller cannot recover this afterwards — the map holds the denial by then")

	// And an ordinary service reports nothing to warn about.
	_, quiet, err := c.prepareEnvVars(ctx, &types.Service{
		ID: "s-2", Name: "api", Namespace: "default",
	}, inst)
	require.NoError(t, err)
	assert.Empty(t, quiet)
}

// The warning's whole history is that it stopped firing without anyone
// noticing, so pin the wiring and not only the value it reads.
func TestRunCreateAttempt_WarnsWhenDenyingRequestedDevices(t *testing.T) {
	logger := log.NewTestLogger()
	st := store.NewBadgerStore(logger)
	require.NoError(t, st.Open(t.TempDir()))
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	testRunner := runner.NewTestRunner()
	mgr := manager.NewTestRunnerManager(nil)
	mgr.SetDockerRunner(testRunner)
	mgr.SetProcessRunner(testRunner)
	c := NewController(st, mgr, logger)
	// NewController derives a component logger, and TestLogger's derived
	// loggers do not share their capture buffer. Point the resolver back at
	// the one this test can read; the call site under test is unchanged.
	c.env.logger = logger

	svc := &types.Service{
		ID: "s-1", Name: "web", Namespace: "default", Scale: 1,
		Env: map[string]string{"NVIDIA_VISIBLE_DEVICES": "all"},
	}
	require.NoError(t, st.Create(ctx, types.ResourceTypeService, svc.Namespace, svc.ID, svc))

	_, err := c.CreateInstance(ctx, svc, "web-0", 0)
	require.NoError(t, err)

	var warned bool
	for _, e := range logger.GetEntries() {
		if e.Message == "Denying GPUs to a service that did not request them" {
			warned = true
		}
	}
	assert.True(t, warned, "the warning stopped firing once without anyone noticing")
}
