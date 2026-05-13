package docker

import (
	"sort"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/runestack/rune/pkg/log"
	runetypes "github.com/runestack/rune/pkg/types"
)

// makeInitTestRunner returns a bare DockerRunner with just the bits
// needed by initStepToContainerConfig (no docker client).
func makeInitTestRunner() *DockerRunner {
	return &DockerRunner{
		logger: log.NewLogger(),
		config: DefaultDockerConfig(),
	}
}

func makeInstanceWithMounts() *runetypes.Instance {
	return &runetypes.Instance{
		ID:          "i-1",
		Name:        "tigerbeetle-0",
		Namespace:   "payments",
		ServiceID:   "svc-1",
		ServiceName: "tigerbeetle",
		Environment: map[string]string{
			"PARENT_VAR": "parent",
			"SHARED":     "from-parent",
		},
		Resources: &runetypes.Resources{
			Memory: runetypes.ResourceLimit{Limit: "1Gi"},
		},
		Metadata: &runetypes.InstanceMetadata{
			Image:     "ghcr.io/tigerbeetle/tigerbeetle:0",
			ImagePull: runetypes.ImagePullMissing,
			VolumeMounts: []runetypes.ResolvedVolumeMount{
				{Name: "data", MountPath: "/data", Source: "/host/data"},
				{Name: "logs", MountPath: "/logs", Source: "/host/logs", ReadOnly: true},
			},
		},
	}
}

func sortedMountTargets(mounts []mount.Mount) []string {
	out := make([]string, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, m.Target)
	}
	sort.Strings(out)
	return out
}

func TestInitStepToContainerConfig_InheritsAllVolumesByDefault(t *testing.T) {
	r := makeInitTestRunner()
	inst := makeInstanceWithMounts()
	step := runetypes.InitStep{
		Name:    "format",
		Image:   "ghcr.io/tigerbeetle/tigerbeetle:0",
		Command: "/tigerbeetle",
		Args:    []string{"format", "/data/0_0.tigerbeetle"},
		// Volumes nil → inherit all parent mounts.
	}

	cfg, host, err := r.initStepToContainerConfig(inst, step)
	require.NoError(t, err)

	// Kubernetes semantics: command → Entrypoint, args → Cmd. This
	// replaces (not appends to) the image's baked-in ENTRYPOINT.
	assert.Equal(t, []string{"/tigerbeetle"}, []string(cfg.Entrypoint))
	assert.Equal(t, []string{"format", "/data/0_0.tigerbeetle"}, []string(cfg.Cmd))
	assert.Equal(t, "init-step", cfg.Labels["rune.kind"])
	assert.Equal(t, "format", cfg.Labels["rune.init.step"])
	assert.Equal(t, []string{"/data", "/logs"}, sortedMountTargets(host.Mounts))
}

// Regression for Bug C: when the parent image declares an ENTRYPOINT
// (e.g. `tini -- /tigerbeetle`), the init step's command must override
// it rather than be appended to it. Pre-fix the runner only set Cmd,
// so Docker prepended the image entrypoint and the binary saw its own
// path as its first argument ("unknown subcommand: '/tigerbeetle'").
func TestInitStepToContainerConfig_CommandReplacesImageEntrypoint(t *testing.T) {
	r := makeInitTestRunner()
	inst := makeInstanceWithMounts()
	step := runetypes.InitStep{
		Name:    "format",
		Image:   "ghcr.io/tigerbeetle/tigerbeetle:latest",
		Command: "/tigerbeetle",
		Args:    []string{"format", "--cluster=0", "/data/0_0.tigerbeetle"},
	}

	cfg, _, err := r.initStepToContainerConfig(inst, step)
	require.NoError(t, err)

	// Entrypoint carries exactly the command; nothing else.
	require.Equal(t, []string{"/tigerbeetle"}, []string(cfg.Entrypoint))
	// And command must NOT appear inside Cmd — that's how the original
	// duplication manifested.
	for _, a := range cfg.Cmd {
		assert.NotEqual(t, "/tigerbeetle", a,
			"command must not be repeated inside args; this is the Bug C duplication")
	}
	assert.Equal(t, []string{"format", "--cluster=0", "/data/0_0.tigerbeetle"}, []string(cfg.Cmd))
}

// Regression for Bug D: when the parent service declares a
// SecurityContext (typically for the main container), init steps
// must inherit it by default — matching how volumes, env and
// secret/configmap mounts inherit. Pre-fix the init step ran with
// Docker's default seccomp profile and TigerBeetle's `format` failed
// with `io_uring is not available`.
func TestInitStepToContainerConfig_InheritsParentSecurityContext(t *testing.T) {
	r := makeInitTestRunner()
	inst := makeInstanceWithMounts()
	inst.SecurityContext = &runetypes.SecurityContext{
		SeccompProfile: &runetypes.SeccompProfile{Type: runetypes.SeccompProfileUnconfined},
		CapAdd:         []string{"SYS_NICE"},
	}
	step := runetypes.InitStep{
		Name:    "format",
		Image:   "ghcr.io/tigerbeetle/tigerbeetle:latest",
		Command: "/tigerbeetle",
		Args:    []string{"format", "/data/0_0.tigerbeetle"},
		// No step.SecurityContext — must fall through to parent.
	}

	_, host, err := r.initStepToContainerConfig(inst, step)
	require.NoError(t, err)

	assert.Contains(t, host.SecurityOpt, "seccomp=unconfined",
		"step must inherit parent service's seccomp=unconfined")
	assert.Equal(t, []string{"SYS_NICE"}, []string(host.CapAdd),
		"step must inherit parent service's capAdd")
}

// Regression for Bug E (Propeller v0.0.1-dev.41 follow-up): the
// SeccompProfile.Type comparison must be case-insensitive and accept
// the Kubernetes-style aliases ("Unconfined", "RuntimeDefault",
// "Localhost"). Pre-v0.0.1-dev.42 the runner did an exact-match
// switch against lowercase "unconfined", so `type: Unconfined`
// silently fell through and the init step ran with the default
// seccomp profile — TigerBeetle's `format` then failed with
// "io_uring is not available".
func TestInitStepToContainerConfig_SeccompProfileTypeIsCaseInsensitive(t *testing.T) {
	r := makeInitTestRunner()
	inst := makeInstanceWithMounts()
	cases := []struct {
		name string
		typ  runetypes.SeccompProfileType
	}{
		{"k8s PascalCase Unconfined", "Unconfined"},
		{"lowercase unconfined", "unconfined"},
		{"mixed-case UnConFiNeD", "UnConFiNeD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst.SecurityContext = &runetypes.SecurityContext{
				SeccompProfile: &runetypes.SeccompProfile{Type: tc.typ},
			}
			step := runetypes.InitStep{
				Name:    "format",
				Image:   "ghcr.io/tigerbeetle/tigerbeetle:latest",
				Command: "/tigerbeetle",
				Args:    []string{"format", "/data/0_0.tigerbeetle"},
			}
			_, host, err := r.initStepToContainerConfig(inst, step)
			require.NoError(t, err)
			assert.Contains(t, host.SecurityOpt, "seccomp=unconfined",
				"type %q should map to seccomp=unconfined regardless of case", tc.typ)
		})
	}
}

// An explicit SecurityContext on the step replaces the parent
// SecurityContext wholesale (no deep merge). Keeps semantics simple
// and predictable for operators.
func TestInitStepToContainerConfig_StepSecurityContextOverridesParent(t *testing.T) {
	r := makeInitTestRunner()
	inst := makeInstanceWithMounts()
	inst.SecurityContext = &runetypes.SecurityContext{
		Privileged: true,
		CapAdd:     []string{"SYS_ADMIN"},
	}
	step := runetypes.InitStep{
		Name:    "lockdown",
		Image:   "busybox",
		Command: "/bin/true",
		SecurityContext: &runetypes.SecurityContext{
			CapDrop: []string{"ALL"},
		},
	}

	_, host, err := r.initStepToContainerConfig(inst, step)
	require.NoError(t, err)

	assert.False(t, host.Privileged,
		"step's explicit SecurityContext must replace the parent's, not merge")
	assert.Empty(t, host.CapAdd,
		"parent CapAdd must not bleed through when step sets its own SecurityContext")
	assert.Equal(t, []string{"ALL"}, []string(host.CapDrop))
}

// A step with no args should still produce a valid Entrypoint and an
// empty (not nil-vs-empty-sensitive) Cmd. Locks the corner case where
// Args is nil.
func TestInitStepToContainerConfig_CommandWithoutArgs(t *testing.T) {
	r := makeInitTestRunner()
	inst := makeInstanceWithMounts()
	step := runetypes.InitStep{
		Name:    "noop",
		Image:   "busybox",
		Command: "/bin/true",
	}

	cfg, _, err := r.initStepToContainerConfig(inst, step)
	require.NoError(t, err)

	assert.Equal(t, []string{"/bin/true"}, []string(cfg.Entrypoint))
	assert.Empty(t, cfg.Cmd)
}

func TestInitStepToContainerConfig_FilterMountsByName(t *testing.T) {
	r := makeInitTestRunner()
	inst := makeInstanceWithMounts()
	step := runetypes.InitStep{
		Name:    "format",
		Image:   "img",
		Command: "/bin/format",
		Volumes: []string{"data"},
	}

	_, host, err := r.initStepToContainerConfig(inst, step)
	require.NoError(t, err)

	require.Len(t, host.Mounts, 1)
	assert.Equal(t, "/data", host.Mounts[0].Target)
	assert.Equal(t, "/host/data", host.Mounts[0].Source)
}

func TestInitStepToContainerConfig_EmptyFilterMountsNothing(t *testing.T) {
	r := makeInitTestRunner()
	inst := makeInstanceWithMounts()
	step := runetypes.InitStep{
		Name:    "warmup",
		Image:   "img",
		Command: "/bin/warmup",
		Volumes: []string{}, // explicit empty
	}

	_, host, err := r.initStepToContainerConfig(inst, step)
	require.NoError(t, err)
	assert.Empty(t, host.Mounts)
}

func TestInitStepToContainerConfig_StepEnvOverridesParent(t *testing.T) {
	r := makeInitTestRunner()
	inst := makeInstanceWithMounts()
	step := runetypes.InitStep{
		Name:    "x",
		Image:   "img",
		Command: "/bin/x",
		Env: map[string]string{
			"SHARED":   "from-step",
			"STEP_VAR": "step",
		},
	}

	cfg, _, err := r.initStepToContainerConfig(inst, step)
	require.NoError(t, err)

	envSet := make(map[string]string, len(cfg.Env))
	for _, kv := range cfg.Env {
		// kv is "K=V"
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				envSet[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	assert.Equal(t, "parent", envSet["PARENT_VAR"])
	assert.Equal(t, "from-step", envSet["SHARED"], "step env must win on conflict")
	assert.Equal(t, "step", envSet["STEP_VAR"])
}

func TestInitStepToContainerConfig_StepResourcesOverrideInstance(t *testing.T) {
	r := makeInitTestRunner()
	inst := makeInstanceWithMounts() // 1Gi mem limit
	step := runetypes.InitStep{
		Name:    "x",
		Image:   "img",
		Command: "/bin/x",
		Resources: &runetypes.Resources{
			Memory: runetypes.ResourceLimit{Limit: "2Gi"},
		},
	}

	_, host, err := r.initStepToContainerConfig(inst, step)
	require.NoError(t, err)
	assert.Equal(t, int64(2*1024*1024*1024), host.Resources.Memory)
}

func TestInitStepToContainerConfig_RejectsBrokenParentMount(t *testing.T) {
	r := makeInitTestRunner()
	inst := makeInstanceWithMounts()
	inst.Metadata.VolumeMounts = []runetypes.ResolvedVolumeMount{
		{Name: "data", MountPath: "/data", Source: ""},
	}
	step := runetypes.InitStep{Name: "x", Image: "img", Command: "/bin/x"}

	_, _, err := r.initStepToContainerConfig(inst, step)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source or mountPath empty")
}

func TestMakeNameFilter(t *testing.T) {
	cases := []struct {
		name   string
		filter []string
		probe  string
		want   bool
	}{
		{"nil filter inherits all", nil, "anything", true},
		{"empty filter excludes all", []string{}, "anything", false},
		{"named filter includes match", []string{"a", "b"}, "a", true},
		{"named filter excludes non-match", []string{"a", "b"}, "c", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := makeNameFilter(tc.filter)(tc.probe)
			assert.Equal(t, tc.want, got)
		})
	}
}
