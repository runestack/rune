package process

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeInitInstance returns a minimal instance suitable for RunInit
// tests. Tests pin Process.WorkingDir to a writable temp dir so the
// step has somewhere to drop sentinel files.
func makeInitInstance(t *testing.T) *types.Instance {
	t.Helper()
	dir := t.TempDir()
	return &types.Instance{
		ID:        "init-test-instance",
		Name:      "init-test",
		NodeID:    "local",
		ServiceID: "init-test-service",
		Namespace: "default",
		Process: &types.ProcessSpec{
			Command:    "true",
			WorkingDir: dir,
		},
	}
}

func TestProcessRunner_RunInit_RejectsBadInputs(t *testing.T) {
	r := newTestRunner(t)
	ctx := context.Background()
	inst := makeInitInstance(t)

	cases := []struct {
		name    string
		step    types.InitStep
		wantSub string
	}{
		{"empty name", types.InitStep{Command: "/bin/true"}, "name is empty"},
		{"empty command", types.InitStep{Name: "x"}, "command is required"},
		{"image set", types.InitStep{Name: "x", Command: "/bin/true", Image: "alpine"}, "image is not supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, err := r.RunInit(ctx, inst, tc.step)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantSub)
			assert.Equal(t, 0, code)
		})
	}

	t.Run("nil instance", func(t *testing.T) {
		_, err := r.RunInit(ctx, nil, types.InitStep{Name: "x", Command: "/bin/true"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nil pointer")
	})
}

func TestProcessRunner_RunInit_HappyPath_ReturnsZero(t *testing.T) {
	r := newTestRunner(t)
	ctx := context.Background()
	inst := makeInitInstance(t)

	code, err := r.RunInit(ctx, inst, types.InitStep{
		Name:    "echo",
		Command: "/bin/sh",
		Args:    []string{"-c", "echo hello"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
}

func TestProcessRunner_RunInit_NonZeroExit_NoError(t *testing.T) {
	r := newTestRunner(t)
	ctx := context.Background()
	inst := makeInitInstance(t)

	code, err := r.RunInit(ctx, inst, types.InitStep{
		Name:    "fail",
		Command: "/bin/sh",
		Args:    []string{"-c", "exit 7"},
	})
	require.NoError(t, err)
	assert.Equal(t, 7, code)
}

func TestProcessRunner_RunInit_RunsInWorkingDir(t *testing.T) {
	r := newTestRunner(t)
	ctx := context.Background()
	inst := makeInitInstance(t)

	// Write a sentinel using the step. The shell `pwd > sentinel`
	// will end up in cmd.Dir == instance.Process.WorkingDir.
	code, err := r.RunInit(ctx, inst, types.InitStep{
		Name:    "writepwd",
		Command: "/bin/sh",
		Args:    []string{"-c", "pwd > sentinel"},
	})
	require.NoError(t, err)
	require.Equal(t, 0, code)

	got, err := os.ReadFile(filepath.Join(inst.Process.WorkingDir, "sentinel"))
	require.NoError(t, err)
	wantDir, err := filepath.EvalSymlinks(inst.Process.WorkingDir)
	require.NoError(t, err)
	assert.Equal(t, wantDir, strings.TrimSpace(string(got)))
}

func TestProcessRunner_RunInit_StepEnvOverridesParent(t *testing.T) {
	r := newTestRunner(t)
	ctx := context.Background()
	inst := makeInitInstance(t)
	inst.Environment = map[string]string{"RUNE_VAR": "from-parent"}

	// The step writes $RUNE_VAR into the sentinel. Step env must win.
	code, err := r.RunInit(ctx, inst, types.InitStep{
		Name:    "envcheck",
		Command: "/bin/sh",
		Args:    []string{"-c", `printf "%s" "$RUNE_VAR" > sentinel`},
		Env:     map[string]string{"RUNE_VAR": "from-step"},
	})
	require.NoError(t, err)
	require.Equal(t, 0, code)

	got, err := os.ReadFile(filepath.Join(inst.Process.WorkingDir, "sentinel"))
	require.NoError(t, err)
	assert.Equal(t, "from-step", string(got))
}

func TestProcessRunner_RunInit_ContextCancellation_ReturnsRuntimeError(t *testing.T) {
	r := newTestRunner(t)
	inst := makeInitInstance(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := r.RunInit(ctx, inst, types.InitStep{
		Name:    "sleeper",
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 30"},
	})
	require.Error(t, err)
	// Either "cancelled" (caught via ctx.Err()) or signal-killed; both
	// are acceptable as a runtime-error rather than a non-zero exit.
	assert.True(t,
		strings.Contains(err.Error(), "cancelled") ||
			strings.Contains(err.Error(), "wait failed") ||
			strings.Contains(err.Error(), "signal"),
		"unexpected err: %v", err)
}

func TestProcessRunner_RunInit_FallsBackToInstanceWorkspace(t *testing.T) {
	// When Process.WorkingDir is empty, RunInit should fall back to
	// the per-instance workspace under baseDir/rune/<ns>/<id> and
	// create it if missing.
	r := newTestRunner(t)
	ctx := context.Background()
	inst := &types.Instance{
		ID:        "fallback-instance",
		Name:      "fallback",
		NodeID:    "local",
		ServiceID: "init-test-service",
		Namespace: "default",
		Process:   &types.ProcessSpec{Command: "true"},
	}

	code, err := r.RunInit(ctx, inst, types.InitStep{
		Name:    "writeworkspace",
		Command: "/bin/sh",
		Args:    []string{"-c", "pwd > sentinel"},
	})
	require.NoError(t, err)
	require.Equal(t, 0, code)

	wantDir := filepath.Join(r.baseDir, "rune", inst.Namespace, inst.ID)
	got, err := os.ReadFile(filepath.Join(wantDir, "sentinel"))
	require.NoError(t, err)
	wantResolved, err := filepath.EvalSymlinks(wantDir)
	require.NoError(t, err)
	assert.Equal(t, wantResolved, strings.TrimSpace(string(got)))
}

func TestTailBuffer_TruncatesToMax(t *testing.T) {
	tb := &tailBuffer{max: 8}
	_, _ = tb.Write([]byte("0123456789ABCDEF"))
	assert.Equal(t, 8, tb.Len())
	assert.Equal(t, "89ABCDEF", tb.String())
}

func TestProcessRunner_RunInit_SkipsOnNonLinuxResource(t *testing.T) {
	// Resources are best-effort: on non-Linux the cgroup setup fails
	// and is logged as a warning; the step must still succeed.
	if runtime.GOOS == "linux" {
		t.Skip("Linux-specific cgroup behaviour covered separately")
	}
	r := newTestRunner(t)
	inst := makeInitInstance(t)
	code, err := r.RunInit(context.Background(), inst, types.InitStep{
		Name:    "rlim",
		Command: "true",
		Resources: &types.Resources{
			CPU:    types.ResourceLimit{Limit: "100m"},
			Memory: types.ResourceLimit{Limit: "64Mi"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
}
