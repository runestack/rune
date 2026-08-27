// Attach surfaces: GetLogs, Exec, RunDebug (inspection sidecar), Dial.
// Split from runner.go (RUNE-312).

package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	runetypes "github.com/runestack/rune/pkg/types"
)

// GetLogs retrieves logs from a container.
func (r *DockerRunner) GetLogs(ctx context.Context, instance *runetypes.Instance, options runner.LogOptions) (io.ReadCloser, error) {
	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get container ID: %w", err)
	}

	// Only set Since/Until when explicitly provided; otherwise a zero time
	// would serialize as "0001-01-01T00:00:00Z" and Docker would interpret
	// Until=year-1 as "show no logs at all".
	var since, until string
	if !options.Since.IsZero() {
		since = options.Since.Format(time.RFC3339Nano)
	}
	if !options.Until.IsZero() {
		until = options.Until.Format(time.RFC3339Nano)
	}

	tail := "all"
	if options.Tail > 0 {
		tail = fmt.Sprintf("%d", options.Tail)
	}

	// Prepare Docker API log options
	logsOptions := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     options.Follow,
		Timestamps: options.Timestamps,
		Since:      since,
		Until:      until,
		Tail:       tail,
	}

	// Get logs from Docker
	logs, err := r.client.ContainerLogs(ctx, containerID, logsOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to get container logs: %w", err)
	}

	// Docker multiplexes stdout and stderr, so we need to demultiplex
	return newLogReader(logs), nil
}

// RunDebug spawns an ephemeral inspection sidecar from the given instance's
// template (image, env, mounts, resources) with the entrypoint overridden to
// `sleep infinity` so the failing app does not re-run. Opens an exec session
// against the sidecar to run options.Command. The sidecar is torn down (stop
// + remove) when the returned ExecStream is Closed.
//
// The caller's instance is treated as a TEMPLATE only — its container is not
// touched. The sidecar gets its own container ID + labels so getContainerID
// lookups never confuse the two.
func (r *DockerRunner) RunDebug(ctx context.Context, instance *runetypes.Instance, options runner.ExecOptions) (runner.ExecStream, error) {
	if instance == nil {
		return nil, fmt.Errorf("invalid instance: nil pointer")
	}
	if len(options.Command) == 0 {
		return nil, fmt.Errorf("debug exec requires a command")
	}

	cfg, hostCfg, err := r.debugSidecarConfig(instance)
	if err != nil {
		return nil, err
	}

	// Replace the entrypoint so the failing app doesn't re-run when we
	// start the container. `sleep infinity` keeps PID 1 alive long
	// enough for the user's exec to attach and inspect state. Clear Cmd
	// so it doesn't add args to sleep.
	cfg.Entrypoint = []string{"sleep", "infinity"}
	cfg.Cmd = nil

	// Tag the sidecar with its own unique instance.id label so
	// getContainerID label lookups never confuse it with the original
	// tombstone (they share a parent instance.id otherwise).
	sidecarID := uuid.New().String()
	if cfg.Labels == nil {
		cfg.Labels = map[string]string{}
	}
	cfg.Labels["rune.debug"] = "true"
	cfg.Labels["rune.instance.id"] = sidecarID
	cfg.Labels["rune.debug.parent"] = instance.ID

	short := sidecarID
	if len(short) >= 8 {
		short = short[:8]
	}
	name := fmt.Sprintf("%s-debug-%s", instance.Name, short)

	// Use missing-pull policy: the failed instance's image is almost
	// certainly already cached locally (it was just running). Skipping
	// "always" avoids a redundant network round-trip when starting a
	// postmortem session.
	debugAnonymous := instance.Metadata != nil && instance.Metadata.ImagePullAnonymous
	if err := r.pullImage(ctx, cfg.Image, runetypes.ImagePullMissing, debugAnonymous); err != nil {
		return nil, fmt.Errorf("pull sidecar image: %w", err)
	}

	resp, err := r.client.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return nil, fmt.Errorf("create sidecar: %w", err)
	}

	// Track cleanup state so an early failure tears the sidecar down
	// and a successful exec defers teardown until the session ends.
	var (
		started   bool
		execOpen  bool
		stopAndRm = func() {
			rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if started {
				timeoutSec := 2
				_ = r.client.ContainerStop(rmCtx, resp.ID, container.StopOptions{Timeout: &timeoutSec})
			}
			_ = r.client.ContainerRemove(rmCtx, resp.ID, container.RemoveOptions{Force: true})
		}
	)
	defer func() {
		if !execOpen {
			stopAndRm()
		}
	}()

	if err := r.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("start sidecar: %w", err)
	}
	started = true

	stream, err := NewDockerExecStream(ctx, r.client, resp.ID, sidecarID, options, r.logger)
	if err != nil {
		return nil, fmt.Errorf("exec into sidecar: %w", err)
	}
	execOpen = true

	r.logger.Info("Spawned debug sidecar",
		log.Str("parent_instance", instance.ID),
		log.Str("sidecar_container", resp.ID),
		log.Str("sidecar_name", name))

	return &cleanupExecStream{ExecStream: stream, cleanup: func() {
		stopAndRm()
		r.logger.Info("Removed debug sidecar",
			log.Str("parent_instance", instance.ID),
			log.Str("sidecar_container", resp.ID))
	}}, nil
}

// cleanupExecStream wraps an ExecStream so a teardown func runs exactly once
// on Close. Used by RunDebug to remove the ephemeral sidecar when the user's
// session ends (clean exit OR client disconnect — the exec service Closes the
// stream in both paths).
type cleanupExecStream struct {
	runner.ExecStream
	cleanup func()
	once    sync.Once
}

func (c *cleanupExecStream) Close() error {
	err := c.ExecStream.Close()
	c.once.Do(c.cleanup)
	return err
}

// Exec creates an interactive exec session with a running container.
func (r *DockerRunner) Exec(ctx context.Context, instance *runetypes.Instance, options runner.ExecOptions) (runner.ExecStream, error) {
	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get container ID: %w", err)
	}

	// Check if the container is running
	container, err := r.client.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}

	if !container.State.Running {
		return nil, fmt.Errorf("container is not running")
	}

	// Create the exec stream
	execStream, err := NewDockerExecStream(ctx, r.client, containerID, instance.ID, options, r.logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create exec stream: %w", err)
	}

	return execStream, nil
}

// Dial opens a TCP connection to the given port on the container's
// network address. Used by `rune port-forward` (RUNE-122). The
// container IP is discovered via the same logic the rest of the
// runner uses (pickContainerIP) so behaviour is consistent with how
// the orchestrator's ingress and networking layers reach containers.
//
// TODO(multi-node): today every container runs on the runed host so
// this dial is local. Once instances can live on other nodes (Release
// 2), this must route through the bound node's agent.
func (r *DockerRunner) Dial(ctx context.Context, instance *runetypes.Instance, port uint32) (net.Conn, error) {
	if instance == nil {
		return nil, fmt.Errorf("nil instance")
	}
	if port == 0 || port > 65535 {
		return nil, fmt.Errorf("invalid port: %d", port)
	}

	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get container ID: %w", err)
	}

	insp, err := r.client.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}
	if !insp.State.Running {
		return nil, fmt.Errorf("container is not running")
	}
	if insp.NetworkSettings == nil {
		return nil, fmt.Errorf("container has no network settings")
	}
	ip := pickContainerIP(insp.NetworkSettings)
	if ip == "" {
		return nil, fmt.Errorf("container has no reachable IP address")
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.FormatUint(uint64(port), 10)))
	if err != nil {
		return nil, fmt.Errorf("dial %s:%d: %w", ip, port, err)
	}
	return conn, nil
}

// debugSidecarConfig builds the postmortem sidecar's container config from
// the instance it sits beside, minus that instance's devices.
//
// A debug sidecar is not the assigned workload. It is usually attached to
// a Failed tombstone, whose reservation was released the moment it was
// tombstoned — so the card may already belong to the replacement, and a
// sidecar that inherited it would sit on a live engine with no ledger row
// naming it. Dropping the request also keeps `rune exec --debug` working
// on a host with no container toolkit.
func (r *DockerRunner) debugSidecarConfig(instance *runetypes.Instance) (*container.Config, *container.HostConfig, error) {
	cfg, hostCfg, err := r.instanceToContainerConfig(instance)
	if err != nil {
		return nil, nil, fmt.Errorf("build sidecar config: %w", err)
	}
	hostCfg.Resources.DeviceRequests = nil
	cfg.Env = formatEnvVars(runner.DeniedEnv(instance.Environment, len(instance.GPUAssignments) > 0))
	return cfg, hostCfg, nil
}
