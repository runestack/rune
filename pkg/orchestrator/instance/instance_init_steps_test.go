package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeInitStepService returns a service with the given init steps,
// already persisted to the store, and an instance template ready to
// pass through CreateInstance.
func makeInitStepService(ctx context.Context, t *testing.T, ts interface{}, name string, steps []types.InitStep) *types.Service {
	t.Helper()
	svc := &types.Service{
		ID:        name,
		Name:      name,
		Namespace: "default",
		Image:     "test:latest",
		Command:   "main",
		Runtime:   types.RuntimeTypeContainer,
		InitSteps: steps,
		Metadata:  &types.ServiceMetadata{Generation: 1},
	}
	return svc
}

// shrinkInitBackoffs swaps in tiny backoffs so retry tests do not
// burn 7 seconds of wall-clock for one failure path. Restored by the
// returned cleanup.
func shrinkInitBackoffs(t *testing.T) {
	t.Helper()
	prevBackoff := initStepOnFailureBackoff
	prevTimeout := initStepDefaultTimeout
	initStepOnFailureBackoff = 1 * time.Millisecond
	initStepDefaultTimeout = 5 * time.Second
	t.Cleanup(func() {
		initStepOnFailureBackoff = prevBackoff
		initStepDefaultTimeout = prevTimeout
	})
}

func TestRunInitSteps_NoSteps_NoOp(t *testing.T) {
	ctx, _, testRunner, controllerIface := setupTestController(t)
	c := controllerIface
	svc := &types.Service{ID: "s1", Namespace: "default"}
	inst := &types.Instance{ID: "i1", ServiceID: "s1", Namespace: "default"}

	err := c.runInitSteps(ctx, testRunner, svc, inst)
	require.NoError(t, err)
	assert.Empty(t, testRunner.InitCalls, "no steps -> no RunInit calls")
	assert.Empty(t, inst.InitStates)
}

func TestRunInitSteps_HappyPath_ThreeSteps(t *testing.T) {
	shrinkInitBackoffs(t)
	ctx, ts, testRunner, controllerIface := setupTestController(t)
	c := controllerIface

	svc := makeInitStepService(ctx, t, ts, "happy", []types.InitStep{
		{Name: "format", Image: "fmt:1", Command: "fmt"},
		{Name: "migrate", Image: "mig:1", Command: "migrate"},
		{Name: "seed", Image: "seed:1", Command: "seed"},
	})
	require.NoError(t, ts.CreateService(ctx, svc))

	inst := &types.Instance{ID: "inst-happy", Name: "inst-happy", ServiceID: svc.ID, Namespace: "default"}
	require.NoError(t, ts.CreateInstance(ctx, inst))

	testRunner.InitExitCode = 0

	err := c.runInitSteps(ctx, testRunner, svc, inst)
	require.NoError(t, err)
	require.Len(t, testRunner.InitCalls, 3)
	assert.Equal(t, "format", testRunner.InitCalls[0].Step.Name)
	assert.Equal(t, "migrate", testRunner.InitCalls[1].Step.Name)
	assert.Equal(t, "seed", testRunner.InitCalls[2].Step.Name)

	require.Len(t, inst.InitStates, 3)
	for _, s := range inst.InitStates {
		assert.Equal(t, types.InitStepStatusSucceeded, s.Status, "step %q", s.Name)
		assert.Equal(t, 1, s.Attempts)
	}
}

func TestRunInitSteps_NeverPolicy_FailsFastNoRetry(t *testing.T) {
	shrinkInitBackoffs(t)
	ctx, ts, testRunner, controllerIface := setupTestController(t)
	c := controllerIface

	svc := makeInitStepService(ctx, t, ts, "never", []types.InitStep{
		{Name: "boom", Image: "x:1", Command: "x", RestartPolicy: types.InitStepRestartNever},
		{Name: "should-not-run", Image: "x:1", Command: "x"},
	})
	require.NoError(t, ts.CreateService(ctx, svc))
	inst := &types.Instance{ID: "inst-never", Name: "inst-never", ServiceID: svc.ID, Namespace: "default"}
	require.NoError(t, ts.CreateInstance(ctx, inst))

	testRunner.InitExitCode = 7

	err := c.runInitSteps(ctx, testRunner, svc, inst)
	require.Error(t, err)
	assert.Equal(t, types.InstanceStatusFailed, inst.Status)
	assert.Contains(t, inst.StatusMessage, `init step "boom"`)

	// Only the first step ran; second was never attempted.
	require.Len(t, testRunner.InitCalls, 1)
	require.Len(t, inst.InitStates, 2)
	assert.Equal(t, types.InitStepStatusFailed, inst.InitStates[0].Status)
	assert.Equal(t, 7, inst.InitStates[0].ExitCode)
	assert.Equal(t, types.InitStepReasonNonZeroExit, inst.InitStates[0].Reason)
	assert.Equal(t, 1, inst.InitStates[0].Attempts)
	assert.Equal(t, types.InitStepStatusPending, inst.InitStates[1].Status)
}

func TestRunInitSteps_OnFailure_RetriesThenSucceeds(t *testing.T) {
	shrinkInitBackoffs(t)
	ctx, ts, testRunner, controllerIface := setupTestController(t)
	c := controllerIface

	svc := makeInitStepService(ctx, t, ts, "retry-ok", []types.InitStep{
		{Name: "flaky", Image: "x:1", Command: "x", RestartPolicy: types.InitStepRestartOnFailure},
	})
	require.NoError(t, ts.CreateService(ctx, svc))
	inst := &types.Instance{ID: "inst-retry-ok", Name: "inst-retry-ok", ServiceID: svc.ID, Namespace: "default"}
	require.NoError(t, ts.CreateInstance(ctx, inst))

	// Fail attempts 1 and 2, succeed on 3.
	testRunner.InitFunc = func(call int, _ types.InitStep) (int, error) {
		if call < 3 {
			return 1, nil
		}
		return 0, nil
	}

	err := c.runInitSteps(ctx, testRunner, svc, inst)
	require.NoError(t, err)
	require.Len(t, testRunner.InitCalls, 3)
	require.Len(t, inst.InitStates, 1)
	assert.Equal(t, types.InitStepStatusSucceeded, inst.InitStates[0].Status)
	assert.Equal(t, 3, inst.InitStates[0].Attempts)
}

func TestRunInitSteps_OnFailure_ExhaustsRetries(t *testing.T) {
	shrinkInitBackoffs(t)
	ctx, ts, testRunner, controllerIface := setupTestController(t)
	c := controllerIface

	svc := makeInitStepService(ctx, t, ts, "retry-fail", []types.InitStep{
		{Name: "always-bad", Image: "x:1", Command: "x"}, // default OnFailure
	})
	require.NoError(t, ts.CreateService(ctx, svc))
	inst := &types.Instance{ID: "inst-retry-fail", Name: "inst-retry-fail", ServiceID: svc.ID, Namespace: "default"}
	require.NoError(t, ts.CreateInstance(ctx, inst))

	testRunner.InitExitCode = 9

	err := c.runInitSteps(ctx, testRunner, svc, inst)
	require.Error(t, err)
	assert.Equal(t, types.InstanceStatusFailed, inst.Status)
	require.Len(t, testRunner.InitCalls, initStepOnFailureMaxAttempts)
	require.Len(t, inst.InitStates, 1)
	assert.Equal(t, types.InitStepStatusFailed, inst.InitStates[0].Status)
	assert.Equal(t, initStepOnFailureMaxAttempts, inst.InitStates[0].Attempts)
	assert.Equal(t, types.InitStepReasonNonZeroExit, inst.InitStates[0].Reason)
}

func TestRunInitSteps_RunIfAlways_RunsEvenWithInitializedVolume(t *testing.T) {
	shrinkInitBackoffs(t)
	ctx, ts, testRunner, controllerIface := setupTestController(t)
	c := controllerIface

	svc := makeInitStepService(ctx, t, ts, "always", []types.InitStep{
		{Name: "tick", Image: "x:1", Command: "x", RunIf: types.RunIf{Type: types.RunIfAlways}},
	})
	require.NoError(t, ts.CreateService(ctx, svc))

	// Seed a volume already initialised for this service to prove that
	// runIf=always still runs.
	vol := &types.Volume{
		ID: "vol-always", Name: "data", Namespace: "default",
		Status:         types.VolumeStatusBound,
		InitializedFor: map[string]time.Time{svc.Namespace + "/" + svc.ID: time.Now()},
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	inst := &types.Instance{
		ID: "inst-always", Name: "inst-always", ServiceID: svc.ID, Namespace: "default",
		Metadata: &types.InstanceMetadata{
			VolumeMounts: []types.ResolvedVolumeMount{
				{Name: "data", VolumeName: vol.Name, VolumeNamespace: vol.Namespace, MountPath: "/data", Source: t.TempDir()},
			},
		},
	}
	require.NoError(t, ts.CreateInstance(ctx, inst))

	err := c.runInitSteps(ctx, testRunner, svc, inst)
	require.NoError(t, err)
	require.Len(t, testRunner.InitCalls, 1, "always-step must run despite initialised parent volume")
}

func TestRunInitSteps_FreshVolume_SkipsWhenInitializedForServicePresent(t *testing.T) {
	shrinkInitBackoffs(t)
	ctx, ts, testRunner, controllerIface := setupTestController(t)
	c := controllerIface

	svc := makeInitStepService(ctx, t, ts, "fresh", []types.InitStep{
		{Name: "format", Image: "x:1", Command: "x"}, // default freshVolume
	})
	require.NoError(t, ts.CreateService(ctx, svc))

	serviceKey := svc.Namespace + "/" + svc.ID
	vol := &types.Volume{
		ID: "vol-fresh", Name: "data", Namespace: "default",
		Status:         types.VolumeStatusBound,
		InitializedFor: map[string]time.Time{serviceKey: time.Now().Add(-time.Hour)},
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	inst := &types.Instance{
		ID: "inst-fresh", Name: "inst-fresh", ServiceID: svc.ID, Namespace: "default",
		Metadata: &types.InstanceMetadata{
			VolumeMounts: []types.ResolvedVolumeMount{
				{Name: "data", VolumeName: vol.Name, VolumeNamespace: vol.Namespace, MountPath: "/data", Source: t.TempDir()},
			},
		},
	}
	require.NoError(t, ts.CreateInstance(ctx, inst))

	err := c.runInitSteps(ctx, testRunner, svc, inst)
	require.NoError(t, err)
	assert.Empty(t, testRunner.InitCalls, "freshVolume must skip when Volume.InitializedFor[serviceKey] is set")
	require.Len(t, inst.InitStates, 1)
	assert.Equal(t, types.InitStepStatusSkipped, inst.InitStates[0].Status)
	assert.Contains(t, inst.InitStates[0].Message, "freshVolume")
}

func TestRunInitSteps_FreshVolume_RunsWhenVolumeNotYetInitialized(t *testing.T) {
	shrinkInitBackoffs(t)
	ctx, ts, testRunner, controllerIface := setupTestController(t)
	c := controllerIface

	svc := makeInitStepService(ctx, t, ts, "first-cast", []types.InitStep{
		{Name: "format", Image: "x:1", Command: "x"},
	})
	require.NoError(t, ts.CreateService(ctx, svc))

	vol := &types.Volume{
		ID: "vol-fresh-run", Name: "data", Namespace: "default",
		Status: types.VolumeStatusBound,
		// no InitializedFor entry
	}
	require.NoError(t, ts.Create(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, vol))

	inst := &types.Instance{
		ID: "inst-fresh-run", Name: "inst-fresh-run", ServiceID: svc.ID, Namespace: "default",
		Metadata: &types.InstanceMetadata{
			VolumeMounts: []types.ResolvedVolumeMount{
				{Name: "data", VolumeName: vol.Name, VolumeNamespace: vol.Namespace, MountPath: "/data", Source: t.TempDir()},
			},
		},
	}
	require.NoError(t, ts.CreateInstance(ctx, inst))

	err := c.runInitSteps(ctx, testRunner, svc, inst)
	require.NoError(t, err)
	require.Len(t, testRunner.InitCalls, 1)

	// Verify the controller stamped the volume after the step succeeded.
	var reloaded types.Volume
	require.NoError(t, ts.Get(ctx, types.ResourceTypeVolume, vol.Namespace, vol.Name, &reloaded))
	serviceKey := svc.Namespace + "/" + svc.ID
	_, ok := reloaded.InitializedFor[serviceKey]
	assert.True(t, ok, "successful init step must stamp Volume.InitializedFor[%q], got %v", serviceKey, reloaded.InitializedFor)
}

func TestRunInitSteps_FileMissing_SkipsWhenSentinelExists(t *testing.T) {
	shrinkInitBackoffs(t)
	ctx, ts, testRunner, controllerIface := setupTestController(t)
	c := controllerIface

	tmp := t.TempDir()
	sentinel := "/data/ready"
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "ready"), []byte("ok"), 0o600))

	svc := makeInitStepService(ctx, t, ts, "fm-skip", []types.InitStep{
		{
			Name: "format", Image: "x:1", Command: "x",
			RunIf: types.RunIf{Type: types.RunIfFileMissing, Path: sentinel, Volume: "data"},
		},
	})
	svc.Volumes = []types.VolumeMount{{
		Name: "data", MountPath: "/data",
		ClaimTemplate: &types.VolumeClaimTemplate{StorageClassName: "local-host", Size: "1Gi", AccessMode: types.AccessModeRWO},
	}}
	require.NoError(t, svc.Validate())
	require.NoError(t, ts.CreateService(ctx, svc))
	inst := &types.Instance{
		ID: "inst-fm-skip", Name: "inst-fm-skip", ServiceID: svc.ID, Namespace: "default",
		Metadata: &types.InstanceMetadata{
			VolumeMounts: []types.ResolvedVolumeMount{
				{Name: "data", MountPath: "/data", Source: tmp},
			},
		},
	}
	require.NoError(t, ts.CreateInstance(ctx, inst))

	err := c.runInitSteps(ctx, testRunner, svc, inst)
	require.NoError(t, err)
	assert.Empty(t, testRunner.InitCalls)
	require.Len(t, inst.InitStates, 1)
	assert.Equal(t, types.InitStepStatusSkipped, inst.InitStates[0].Status)
}

// A subPath mount binds <source>/<subPath> at MountPath, so the sentinel
// lives one level deeper on the host than the container path suggests.
func TestRunInitSteps_FileMissing_SkipsWhenSentinelExistsUnderSubPath(t *testing.T) {
	shrinkInitBackoffs(t)
	ctx, ts, testRunner, controllerIface := setupTestController(t)
	c := controllerIface

	tmp := t.TempDir()
	sentinel := "/data/ready"
	require.NoError(t, os.Mkdir(filepath.Join(tmp, "pg"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "pg", "ready"), []byte("ok"), 0o600))

	svc := makeInitStepService(ctx, t, ts, "fm-sub", []types.InitStep{
		{
			Name: "format", Image: "x:1", Command: "x",
			RunIf: types.RunIf{Type: types.RunIfFileMissing, Path: sentinel, Volume: "data"},
		},
	})
	svc.Volumes = []types.VolumeMount{{
		Name: "data", MountPath: "/data", SubPath: "pg",
		ClaimTemplate: &types.VolumeClaimTemplate{StorageClassName: "local-host", Size: "1Gi", AccessMode: types.AccessModeRWO},
	}}
	require.NoError(t, svc.Validate())
	require.NoError(t, ts.CreateService(ctx, svc))
	inst := &types.Instance{
		ID: "inst-fm-sub", Name: "inst-fm-sub", ServiceID: svc.ID, Namespace: "default",
		Metadata: &types.InstanceMetadata{
			VolumeMounts: []types.ResolvedVolumeMount{
				{Name: "data", MountPath: "/data", Source: tmp, SubPath: "pg"},
			},
		},
	}
	require.NoError(t, ts.CreateInstance(ctx, inst))

	err := c.runInitSteps(ctx, testRunner, svc, inst)
	require.NoError(t, err)
	assert.Empty(t, testRunner.InitCalls)
	require.Len(t, inst.InitStates, 1)
	assert.Equal(t, types.InitStepStatusSkipped, inst.InitStates[0].Status)
}

func TestRunInitSteps_FileMissing_RunsWhenSentinelAbsent(t *testing.T) {
	shrinkInitBackoffs(t)
	ctx, ts, testRunner, controllerIface := setupTestController(t)
	c := controllerIface

	tmp := t.TempDir()
	sentinel := "/data/ready"
	svc := makeInitStepService(ctx, t, ts, "fm-run", []types.InitStep{
		{
			Name: "format", Image: "x:1", Command: "x",
			RunIf: types.RunIf{Type: types.RunIfFileMissing, Path: sentinel, Volume: "data"},
		},
	})
	svc.Volumes = []types.VolumeMount{{
		Name: "data", MountPath: "/data",
		ClaimTemplate: &types.VolumeClaimTemplate{StorageClassName: "local-host", Size: "1Gi", AccessMode: types.AccessModeRWO},
	}}
	require.NoError(t, svc.Validate())
	require.NoError(t, ts.CreateService(ctx, svc))
	inst := &types.Instance{
		ID: "inst-fm-run", Name: "inst-fm-run", ServiceID: svc.ID, Namespace: "default",
		Metadata: &types.InstanceMetadata{
			VolumeMounts: []types.ResolvedVolumeMount{
				{Name: "data", MountPath: "/data", Source: tmp},
			},
		},
	}
	require.NoError(t, ts.CreateInstance(ctx, inst))

	err := c.runInitSteps(ctx, testRunner, svc, inst)
	require.NoError(t, err)
	require.Len(t, testRunner.InitCalls, 1)
}

func TestRunInitSteps_ErrInitNotSupported_FailsImmediately(t *testing.T) {
	shrinkInitBackoffs(t)
	ctx, ts, testRunner, controllerIface := setupTestController(t)
	c := controllerIface

	svc := makeInitStepService(ctx, t, ts, "unsupported", []types.InitStep{
		{Name: "step", Image: "x:1", Command: "x"},
	})
	require.NoError(t, ts.CreateService(ctx, svc))
	inst := &types.Instance{ID: "inst-unsupported", Name: "inst-unsupported", ServiceID: svc.ID, Namespace: "default"}
	require.NoError(t, ts.CreateInstance(ctx, inst))

	testRunner.InitFunc = func(int, types.InitStep) (int, error) {
		return 0, runner.ErrInitNotSupported
	}

	err := c.runInitSteps(ctx, testRunner, svc, inst)
	require.Error(t, err)
	assert.True(t, errors.Is(err, runner.ErrInitNotSupported))
	require.Len(t, testRunner.InitCalls, 1, "ErrInitNotSupported must not retry")
	assert.Equal(t, types.InstanceStatusFailed, inst.Status)
	assert.Equal(t, types.InitStepStatusFailed, inst.InitStates[0].Status)
	assert.Equal(t, types.InitStepReasonRuntimeError, inst.InitStates[0].Reason)
}
