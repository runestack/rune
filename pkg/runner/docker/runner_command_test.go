package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/runestack/rune/pkg/log"
	runetypes "github.com/runestack/rune/pkg/types"
)

// makeRunnerForCommandTests returns a bare DockerRunner with just the
// fields needed by instanceToContainerConfig (no docker client).
func makeRunnerForCommandTests() *DockerRunner {
	return &DockerRunner{
		logger: log.NewLogger(),
		config: DefaultDockerConfig(),
	}
}

// makeMainInstance returns an Instance fixture with the minimum
// metadata instanceToContainerConfig needs (Image set).
func makeMainInstance() *runetypes.Instance {
	return &runetypes.Instance{
		ID:          "i-1",
		Name:        "api-0",
		Namespace:   "app",
		ServiceID:   "svc-1",
		ServiceName: "api",
		Environment: map[string]string{"FOO": "bar"},
		Metadata: &runetypes.InstanceMetadata{
			Image: "nginx:alpine",
		},
	}
}

// Bug F (regression): Service.Command and Service.Args must reach the
// main container as Entrypoint and Cmd. Pre-v0.0.1-dev.43 the runner
// silently dropped both, so Docker fell back to the image's baked
// ENTRYPOINT / CMD and any args declared in the cast file were
// invisible to the container. TigerBeetle's `start --addresses=...`
// notably degraded into a no-arg `tigerbeetle` invocation.
func TestInstanceToContainerConfig_CommandAndArgsReachMainContainer(t *testing.T) {
	r := makeRunnerForCommandTests()
	inst := makeMainInstance()
	inst.Metadata.Command = "/tigerbeetle"
	inst.Metadata.Args = []string{"start", "--addresses=0.0.0.0:4000", "/data/0_0.tigerbeetle"}

	cfg, _, err := r.instanceToContainerConfig(inst)
	require.NoError(t, err)

	assert.Equal(t, []string{"/tigerbeetle"}, []string(cfg.Entrypoint),
		"Service.Command must become Entrypoint, replacing the image's ENTRYPOINT")
	assert.Equal(t, []string{"start", "--addresses=0.0.0.0:4000", "/data/0_0.tigerbeetle"}, []string(cfg.Cmd),
		"Service.Args must become Cmd, replacing the image's CMD")
}

// args without command: leaves Entrypoint untouched so the image's
// ENTRYPOINT keeps running, but supplies positional args via Cmd.
// This is the canonical "trust the image's entrypoint, just pass args"
// pattern users reach for when an image bakes in a sensible binary.
func TestInstanceToContainerConfig_ArgsOnlyKeepsImageEntrypoint(t *testing.T) {
	r := makeRunnerForCommandTests()
	inst := makeMainInstance()
	inst.Metadata.Args = []string{"-c", "echo hi"}

	cfg, _, err := r.instanceToContainerConfig(inst)
	require.NoError(t, err)

	assert.Nil(t, cfg.Entrypoint, "no Service.Command → image's ENTRYPOINT must be preserved")
	assert.Equal(t, []string{"-c", "echo hi"}, []string(cfg.Cmd))
}

// Backwards-compat: instances with neither command nor args produce
// Entrypoint=nil and Cmd=nil, so Docker falls back entirely to the
// image's baked-in ENTRYPOINT and CMD. This is the path most existing
// services follow today.
func TestInstanceToContainerConfig_NoCommandOrArgsFallsBackToImage(t *testing.T) {
	r := makeRunnerForCommandTests()
	inst := makeMainInstance()

	cfg, _, err := r.instanceToContainerConfig(inst)
	require.NoError(t, err)

	assert.Nil(t, cfg.Entrypoint, "image's ENTRYPOINT must be preserved when spec is silent")
	assert.Empty(t, cfg.Cmd, "image's CMD must be preserved when spec is silent")
}

// `rune exec` uses instance.Exec.Command for ad-hoc overrides. That
// path must still win over the spec's Command/Args — otherwise we'd
// be running the service's normal entrypoint instead of the user's
// chosen exec command.
func TestInstanceToContainerConfig_ExecOverridesSpecCommandAndArgs(t *testing.T) {
	r := makeRunnerForCommandTests()
	inst := makeMainInstance()
	inst.Metadata.Command = "/tigerbeetle"
	inst.Metadata.Args = []string{"start", "/data/0_0.tigerbeetle"}
	inst.Exec = &runetypes.Exec{
		Command: []string{"/bin/sh", "-c", "echo from-exec"},
	}

	cfg, _, err := r.instanceToContainerConfig(inst)
	require.NoError(t, err)

	assert.Nil(t, cfg.Entrypoint,
		"Exec must reset Entrypoint so the user's exec command is what actually runs")
	assert.Equal(t, []string{"/bin/sh", "-c", "echo from-exec"}, []string(cfg.Cmd),
		"Exec.Command must replace spec Args entirely")
}
