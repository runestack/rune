// Package docker provides a Docker-based implementation of the runner interface.
package docker

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	imageTypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/docker/registryauth"
	"github.com/runestack/rune/pkg/types"
	runetypes "github.com/runestack/rune/pkg/types"
)

// DockerConfig holds Docker runner configuration options
type DockerConfig struct {
	// APIVersion is the Docker API version to use
	// If empty, auto-negotiation will be used
	APIVersion string

	// FallbackAPIVersion is used when auto-negotiation fails
	// Default is "1.43" which is widely compatible
	FallbackAPIVersion string

	// Timeout for API version negotiation in seconds
	NegotiationTimeoutSeconds int

	// Permissions for mounts created on the host before binding into containers
	// If zero, sensible defaults will be used.
	SecretDirMode  os.FileMode
	SecretFileMode os.FileMode
	ConfigDirMode  os.FileMode
	ConfigFileMode os.FileMode

	// Registry authentication configuration loaded from runefile
	Registries []RegistryConfig
}

// DefaultDockerConfig returns the default Docker configuration
func DefaultDockerConfig() *DockerConfig {
	return &DockerConfig{
		APIVersion:                "",     // Empty means use auto-negotiation
		FallbackAPIVersion:        "1.43", // Fallback to a widely compatible version
		NegotiationTimeoutSeconds: 3,
		// Defaults optimized for Docker Desktop on macOS/Windows where
		// FUSE permissions can otherwise block container access.
		SecretDirMode:  0o755,
		SecretFileMode: 0o444,
		ConfigDirMode:  0o755,
		ConfigFileMode: 0o644,
		Registries:     nil,
	}
}

// RegistryConfig defines a registry auth entry
type RegistryConfig struct {
	Name     string       `mapstructure:"name"`
	Registry string       `mapstructure:"registry"`
	Auth     RegistryAuth `mapstructure:"auth"`
}

// RegistryAuth defines supported auth types
type RegistryAuth struct {
	Type     string `mapstructure:"type"` // basic | token | ecr
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	Token    string `mapstructure:"token"`
	Region   string `mapstructure:"region"`
}

// Validate that DockerRunner implements the runner.Runner interface
var _ runner.Runner = &DockerRunner{}

// DockerRunner implements the runner.Runner interface for Docker.
type DockerRunner struct {
	client       *client.Client
	logger       log.Logger
	config       *DockerConfig
	ecrAuthCache map[string]ecrAuthEntry
	providers    []registryauth.Provider

	// dnsMu guards dnsServers + dnsSearch which are toggled at
	// runtime by the agent once the data path (RUNE-041) is healthy
	// and the embedded DNS server (RUNE-063) has bound.
	dnsMu      sync.RWMutex
	dnsServers []string
	dnsSearch  []string
}

func (r *DockerRunner) Type() types.RunnerType {
	return types.RunnerTypeDocker
}

// SetDNSInjection toggles --dns/--dns-search injection on subsequent
// ContainerCreate calls. The agent calls this after the embedded DNS
// server has bound (RUNE-063) and the data plane is healthy
// (RUNE-041). Pass nil/empty servers to disable injection.
//
// Safe to call concurrently with container creates: only future
// creates pick up the new value.
func (r *DockerRunner) SetDNSInjection(servers []string, search []string) {
	r.dnsMu.Lock()
	defer r.dnsMu.Unlock()
	r.dnsServers = append([]string(nil), servers...)
	r.dnsSearch = append([]string(nil), search...)
}

// NewDockerRunner creates a new DockerRunner instance.
func NewDockerRunner(logger log.Logger) (*DockerRunner, error) {
	// Use default configuration
	config := DefaultDockerConfig()
	return NewDockerRunnerWithConfig(logger, config)
}

// NewDockerRunnerWithConfig creates a new DockerRunner with specific configuration.
func NewDockerRunnerWithConfig(logger log.Logger, config *DockerConfig) (*DockerRunner, error) {
	// Default to use global logger if none provided
	if logger == nil {
		logger = log.GetDefaultLogger().WithComponent("docker-runner")
	}

	// Use default config if none provided
	if config == nil {
		config = DefaultDockerConfig()
	}

	// Create a client with the appropriate API version
	client, err := createClientWithVersionHandling(logger, config)
	if err != nil {
		return nil, err
	}

	return &DockerRunner{
		client:       client,
		logger:       logger,
		config:       config,
		ecrAuthCache: make(map[string]ecrAuthEntry),
		providers:    nil,
	}, nil
}

type ecrAuthEntry struct {
	Username string
	Password string
	Expires  time.Time
}

// createClientWithVersionHandling creates a Docker client with appropriate API version handling
func createClientWithVersionHandling(logger log.Logger, config *DockerConfig) (*client.Client, error) {
	// Create Docker client with default configuration
	dockerClient, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	// If a specific API version is configured, use it
	if config.APIVersion != "" {
		logger.Info("Using specified Docker API version",
			log.Str("api_version", config.APIVersion))

		dockerClient, err = client.NewClientWithOpts(
			client.FromEnv,
			client.WithVersion(config.APIVersion),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create Docker client with version %s: %w", config.APIVersion, err)
		}

		return dockerClient, nil
	}

	// Otherwise try to negotiate API version safely with timeout
	negotiationTimeout := time.Duration(config.NegotiationTimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), negotiationTimeout)
	defer cancel()

	dockerClient.NegotiateAPIVersion(ctx)
	clientVersion := dockerClient.ClientVersion()
	logger.Info("Using negotiated Docker API version", log.Str("api_version", clientVersion))

	// Verify the version works by doing a ping test
	dockerClient, err = verifyClientCompatibility(dockerClient, clientVersion, config.FallbackAPIVersion, logger)
	if err != nil {
		return nil, err
	}

	return dockerClient, nil
}

// verifyClientCompatibility checks if the Docker client is compatible with the server
// and falls back to a compatible version if needed.
// If a fallback is required, the old client is closed and a new one is returned.
func verifyClientCompatibility(dockerClient *client.Client, clientVersion, fallbackVersion string, logger log.Logger) (*client.Client, error) {
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pingCancel()

	_, err := dockerClient.Ping(pingCtx)

	// Check for version mismatch errors
	if err != nil && strings.Contains(err.Error(), "client version") &&
		strings.Contains(err.Error(), "too new") {
		// If we get version mismatch error, use the fallback version
		logger.Warn("Docker API version mismatch, falling back to compatibility version",
			log.Str("current_version", clientVersion),
			log.Str("fallback_version", fallbackVersion),
			log.Err(err))

		// Close the incompatible client
		_ = dockerClient.Close()

		// Create new client with fallback version
		newClient, err := client.NewClientWithOpts(
			client.FromEnv,
			client.WithVersion(fallbackVersion),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create Docker client with fallback version %s: %w",
				fallbackVersion, err)
		}

		return newClient, nil
	} else if err != nil {
		// If there's a non-version error, log it but continue
		logger.Warn("Docker ping error (continuing anyway)", log.Err(err))
	}

	return dockerClient, nil
}

// Create creates a new container but does not start it. The container name
// is `<namespace>-<instance.Name>-<id_prefix>` where id_prefix starts at 8
// hex chars of the instance UUID and extends only when docker reports a
// name collision — git-style abbreviation. The UUID suffix:
//
//   - Eliminates cross-namespace collisions (the `landing-0` bug):
//     prod-landing-0-<id> and dev-landing-0-<id> never collide.
//   - Eliminates the tombstone-rename dance: a Failed instance keeps its
//     container forever, and the freshly-created replacement gets its
//     own UUID-suffixed name without colliding.
//   - Stays human-readable: `docker ps` shows
//     "prod-landing-0-5d68e677" which encodes ns/service/ordinal/id.
//
// Container lookups in this runner go through `getContainerID` which
// prefers `instance.ContainerID` over labels, so dynamic-length names do
// not complicate any code path.
func (r *DockerRunner) Create(ctx context.Context, instance *runetypes.Instance) error {
	if instance == nil {
		return fmt.Errorf("invalid instance: nil pointer")
	}

	// Create container config and host config
	containerConfig, hostConfig, err := r.instanceToContainerConfig(instance)
	if err != nil {
		return fmt.Errorf("failed to create container configuration: %w", err)
	}

	// Pull the image first
	pullPolicy := runetypes.ImagePullAlways
	if instance.Metadata != nil && instance.Metadata.ImagePull != "" {
		pullPolicy = instance.Metadata.ImagePull
	}
	if err := r.pullImage(ctx, containerConfig.Image, pullPolicy); err != nil {
		return fmt.Errorf("failed to pull image %s: %w", containerConfig.Image, err)
	}

	resp, name, err := r.createContainerWithUniqueSuffix(ctx, containerConfig, hostConfig, instance)
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	instance.ContainerID = resp.ID

	r.logger.Info("Created container for instance",
		log.Str("container_id", resp.ID),
		log.Str("container_name", name),
		log.Str("instance_id", instance.ID))

	return nil
}

// createContainerWithUniqueSuffix builds a container name of the form
// `<namespace>-<instance.Name>-<id_prefix>` and creates the container,
// extending id_prefix from 8 to 32 hex chars on docker name-conflict. The
// vast majority of calls succeed at 8 chars on the first attempt; only true
// UUID-prefix collisions within the same ns/service/ordinal scope on the
// same host force extension. Capped at 32 (full UUID, no dashes) for safety.
//
// Returns the create response and the final container name actually used.
func (r *DockerRunner) createContainerWithUniqueSuffix(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig, instance *runetypes.Instance) (container.CreateResponse, string, error) {
	prefix := fmt.Sprintf("%s-%s-", instance.Namespace, instance.Name)
	fullID := strings.ReplaceAll(instance.ID, "-", "") // 32 hex chars

	const minSuffix = 8
	for n := minSuffix; n <= len(fullID); n++ {
		name := prefix + fullID[:n]
		resp, err := r.client.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
		if err == nil {
			return resp, name, nil
		}
		if !isNameConflictError(err) {
			return container.CreateResponse{}, "", err
		}
		// Name collision — log once and extend the suffix on next iteration.
		r.logger.Warn("Container name collision; extending suffix",
			log.Str("attempted_name", name),
			log.Str("instance_id", instance.ID),
			log.Int("next_suffix_len", n+1))
	}
	return container.CreateResponse{}, "", fmt.Errorf("name suffix exhausted at %d chars for instance %s (?!)", len(fullID), instance.ID)
}

// isNameConflictError returns true if the error indicates a Docker name conflict
func isNameConflictError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "is already in use by container") || strings.Contains(msg, "Conflict. The container name")
}

// Start starts an existing container.
func (r *DockerRunner) Start(ctx context.Context, instance *runetypes.Instance) error {
	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to get container ID: %w", err)
	}

	// Start the container - using empty StartOptions as we don't need any special configuration
	if err := r.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	// Record the container's IP on its primary network. Endpoint
	// publishing and VIP routing (RUNE-063) key off this — an empty
	// ContainerIP means the service's VIP proxy has no backend and
	// resets every connection. Docker assigns the bridge IP during
	// start, but an inspect issued in the same instant can race ahead
	// of it, so poll briefly rather than inspecting once.
	if instance.Metadata == nil {
		instance.Metadata = &runetypes.InstanceMetadata{}
	}
	if ip := r.waitContainerIP(ctx, containerID); ip != "" {
		instance.Metadata.ContainerIP = ip
	} else {
		r.logger.Warn("Started container but could not determine its IP; endpoints/VIP routing will be degraded",
			log.Str("container_id", containerID),
			log.Str("instance_id", instance.ID))
	}

	r.logger.Info("Started container for instance",
		log.Str("container_id", containerID),
		log.Str("instance_id", instance.ID),
		log.Str("instance_name", instance.Name),
		log.Str("instance_status", string(instance.Status)))

	return nil
}

// Stop stops a running container.
func (r *DockerRunner) Stop(ctx context.Context, instance *runetypes.Instance, timeout time.Duration) error {
	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to get container ID: %w", err)
	}

	// Convert timeout to seconds
	timeoutSeconds := int(timeout.Seconds())

	// Stop the container
	if err := r.client.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeoutSeconds}); err != nil {
		return fmt.Errorf("failed to stop container: %w", err)
	}

	r.logger.Info("Stopped container for instance",
		log.Str("container_id", containerID),
		log.Str("instance_id", instance.ID))

	return nil
}

// Remove removes a container.
func (r *DockerRunner) Remove(ctx context.Context, instance *runetypes.Instance, force bool) error {
	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to get container ID: %w", err)
	}

	// Remove the container
	options := container.RemoveOptions{
		Force: force,
	}

	if err := r.client.ContainerRemove(ctx, containerID, options); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	r.logger.Info("Removed container for instance",
		log.Str("container_id", containerID),
		log.Str("instance_id", instance.ID))

	return nil
}

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

// Status retrieves the current status of a container.
func (r *DockerRunner) Status(ctx context.Context, instance *runetypes.Instance) (runetypes.InstanceStatus, error) {
	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return "", fmt.Errorf("failed to get container ID: %w", err)
	}

	// Get container information
	container, err := r.client.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("failed to inspect container: %w", err)
	}

	// Map Docker state to Rune instance status
	if container.State.Running {
		return runetypes.InstanceStatusRunning, nil
	} else if container.State.ExitCode != 0 {
		return runetypes.InstanceStatusFailed, nil
	} else {
		return runetypes.InstanceStatusStopped, nil
	}
}

// List lists all service instances managed by this runner.
func (r *DockerRunner) List(ctx context.Context, namespace string) ([]*runetypes.Instance, error) {
	// Filter containers managed by this runner
	args := filters.NewArgs(
		filters.Arg("label", "rune.managed=true"),
	)

	if namespace != "" {
		args.Add("label", "rune.namespace="+namespace)
	}

	containers, err := r.client.ContainerList(ctx, container.ListOptions{
		All:     true, // Include stopped containers
		Filters: args,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	// Convert Docker API types to Rune types
	instances := make([]*runetypes.Instance, 0, len(containers))

	for _, c := range containers {
		// Extract instance information from container labels
		instanceID := c.Labels["rune.instance.id"]
		if instanceID == "" {
			r.logger.Warn("Found container without instance ID",
				log.Str("container_id", c.ID),
				log.Str("container_name", c.Names[0]))
			continue
		}

		// Create instance object. ServiceName is required for the
		// reconciler's orphan detection to match a running container to
		// its service; without it, replaced containers are never reaped.
		instance := &runetypes.Instance{
			ID:          instanceID,
			ContainerID: c.ID,
			Name:        c.Names[0][1:], // Remove leading slash from container name
			ServiceID:   c.Labels["rune.service.id"],
			ServiceName: c.Labels["rune.service.name"],
			NodeID:      "local", // Assume local node for now
		}

		// Set status based on container state
		switch c.State {
		case "running":
			instance.Status = runetypes.InstanceStatusRunning
		case "exited":
			// For exited containers, we need to inspect to get the exit code
			inspect, err := r.client.ContainerInspect(ctx, c.ID)
			if err == nil && inspect.State.ExitCode != 0 {
				instance.Status = runetypes.InstanceStatusFailed
			} else {
				instance.Status = runetypes.InstanceStatusStopped
			}
		case "created":
			instance.Status = runetypes.InstanceStatusPending
		default:
			instance.Status = ""
		}

		instances = append(instances, instance)
	}

	return instances, nil
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

	cfg, hostCfg, err := r.instanceToContainerConfig(instance)
	if err != nil {
		return nil, fmt.Errorf("build sidecar config: %w", err)
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
	if err := r.pullImage(ctx, cfg.Image, runetypes.ImagePullMissing); err != nil {
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

// getContainerID gets the container ID for an instance.
//
// Prefers instance.ContainerID when set so failed-instance retention
// tombstones (which share their label-scoped instance.id with a freshly-
// recreated replacement container during the brief window before the GC
// reclaims them) can be addressed unambiguously. Falls back to label
// lookup only on first creation or after a server restart that lost
// the in-memory ContainerID.
func (r *DockerRunner) getContainerID(ctx context.Context, instance *runetypes.Instance) (string, error) {
	if instance.ContainerID != "" {
		return instance.ContainerID, nil
	}
	// Try to get the container directly from the instance ID using labels
	args := filters.NewArgs(
		filters.Arg("label", "rune.managed=true"),
		filters.Arg("label", "rune.instance.id="+instance.ID),
	)

	if instance.Namespace != "" {
		args.Add("label", "rune.namespace="+instance.Namespace)
	}

	containers, err := r.client.ContainerList(ctx, container.ListOptions{
		All:     true, // Include stopped containers
		Filters: args,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list containers: %w", err)
	}

	if len(containers) == 0 {
		return "", fmt.Errorf("no container found for instance ID: %s", instance.ID)
	}

	if len(containers) > 1 {
		r.logger.Warn("Multiple containers found for instance ID",
			log.Str("instance_id", instance.ID),
			log.Int("container_count", len(containers)))
	}

	// Return the first matching container
	return containers[0].ID, nil
}

// pullImage pulls an image from the registry, honoring the supplied
// imagePull mode ("always", "missing", "never"). Empty defaults to
// "always". For "never", no pull is attempted; container creation will
// fail later if the image is missing locally.
func (r *DockerRunner) pullImage(ctx context.Context, image string, policy string) error {
	switch policy {
	case runetypes.ImagePullNever:
		r.logger.Debug("Skipping image pull (imagePull=never)", log.Str("image", image))
		return nil
	case runetypes.ImagePullMissing:
		if _, _, err := r.client.ImageInspectWithRaw(ctx, image); err == nil {
			r.logger.Debug("Image present locally; skipping pull (imagePull=missing)", log.Str("image", image))
			return nil
		}
	case runetypes.ImagePullAlways, "":
		// fall through and re-pull every time
	default:
		// Unknown values are treated as always but logged.
		r.logger.Warn("Unknown imagePull value; defaulting to always",
			log.Str("imagePull", policy), log.Str("image", image))
	}

	r.logger.Info("Pulling Docker image",
		log.Str("image", image), log.Str("policy", policy))

	// Resolve registry auth for this image if configured
	host := parseImageHost(image)
	registryAuth := r.resolveRegistryAuth(image)
	if registryAuth == "" {
		r.logger.Debug("No registry auth resolved for image",
			log.Str("image", image),
			log.Str("host", host))
	} else {
		r.logger.Debug("Resolved registry auth for image",
			log.Str("image", image),
			log.Str("host", host))
	}

	// Pull the image
	reader, err := r.client.ImagePull(ctx, image, imageTypes.PullOptions{RegistryAuth: registryAuth})
	if err != nil {
		return err
	}
	defer reader.Close()

	// Read the output to complete the pull
	_, err = io.Copy(io.Discard, reader)
	return err
}

// resolveRegistryAuth selects an auth entry based on image host and encodes it for Docker ImagePull
func (r *DockerRunner) resolveRegistryAuth(imageRef string) string {
	host := parseImageHost(imageRef)
	if host == "" {
		return ""
	}
	// Provider-based resolution (lazily built)
	if r.providers == nil {
		var regs []map[string]any
		for _, rc := range r.config.Registries {
			regs = append(regs, map[string]any{
				"registry": rc.Registry,
				"auth": map[string]any{
					"type":     rc.Auth.Type,
					"username": rc.Auth.Username,
					"password": rc.Auth.Password,
					"token":    rc.Auth.Token,
					"region":   rc.Auth.Region,
				},
			})
		}
		r.providers = registryauth.BuildProviders(context.Background(), regs)
	}
	for _, p := range r.providers {
		if p.Match(host) {
			if auth, _ := p.Resolve(context.Background(), host, imageRef); auth != "" {
				return auth
			}
		}
	}
	return ""
}

func parseImageHost(imageRef string) string {
	// Format examples:
	// ghcr.io/owner/repo:tag
	// 123456789012.dkr.ecr.us-east-1.amazonaws.com/repo:tag
	// nginx:alpine (Docker Hub)
	// If no '/' present, it's a library image on Docker Hub
	if !strings.Contains(imageRef, "/") {
		return "index.docker.io"
	}
	parts := strings.Split(imageRef, "/")
	first := parts[0]
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		return first
	}
	// registry not explicit -> Docker Hub
	return "index.docker.io"
}

func matchWildcardHost(pattern, host string) bool {
	// very simple wildcard: "*.domain.tld" -> suffix match without leading dot constraint
	if !strings.Contains(pattern, "*") {
		return strings.EqualFold(pattern, host)
	}
	// split on first '*'
	idx := strings.Index(pattern, "*")
	suffix := pattern[idx+1:]
	return strings.HasSuffix(host, suffix)
}

// instanceToContainerConfig converts a Rune instance to Docker container config.
func (r *DockerRunner) instanceToContainerConfig(instance *runetypes.Instance) (*container.Config, *container.HostConfig, error) {
	// Extract service ID from the instance
	serviceID := instance.ServiceID
	if serviceID == "" {
		return nil, nil, fmt.Errorf("service ID is required")
	}

	// Get the image from environment variables
	var image string
	if instance.Metadata != nil {
		image = instance.Metadata.Image
	}

	// Validate that we have an image
	if image == "" {
		return nil, nil, fmt.Errorf("no image specified for instance %s", instance.ID)
	}

	r.logger.Debug("Using image for instance",
		log.Str("instance", instance.ID),
		log.Str("image", image))

	// Configure the container
	containerConfig := &container.Config{
		Image: image,
		Labels: map[string]string{
			"rune.managed":     "true",
			"rune.namespace":   instance.Namespace,
			"rune.instance.id": instance.ID,
			"rune.service.id":  serviceID,
			// Service name, NOT the instance name. The orphan reaper
			// matches running containers to their service by this value
			// (via Instance.ServiceName reconstructed in List); storing
			// the instance name here left replaced containers unreapable.
			"rune.service.name":  instance.ServiceName,
			"rune.instance.name": instance.Name,
		},
		Env: formatEnvVars(instance.Environment),
	}

	// Apply the service's command/args to the container.
	// Service.Command → Entrypoint (replaces the image's ENTRYPOINT);
	// Service.Args → Cmd (replaces the image's CMD).
	// Empty fields leave the image's baked-in defaults untouched.
	//
	// instance.Exec.Command, when set, is a `rune exec` ad-hoc
	// override and takes precedence over the spec — it replaces both
	// Entrypoint and Cmd with the literal command line the user
	// passed to `rune exec`.
	if instance.Metadata != nil {
		if instance.Metadata.Command != "" {
			containerConfig.Entrypoint = []string{instance.Metadata.Command}
		}
		if len(instance.Metadata.Args) > 0 {
			containerConfig.Cmd = append([]string(nil), instance.Metadata.Args...)
		}
	}
	if instance.Exec != nil && len(instance.Exec.Command) > 0 {
		containerConfig.Entrypoint = nil
		containerConfig.Cmd = instance.Exec.Command
	}

	// Configure host config with mounts and resources
	hostConfig := &container.HostConfig{}

	// Map resource requests/limits to Docker host config if provided
	if instance != nil && instance.Resources != nil {
		cpuReqCores, _ := runetypes.ParseCPU(instance.Resources.CPU.Request)
		cpuLimCores, _ := runetypes.ParseCPU(instance.Resources.CPU.Limit)
		memReqBytes, _ := runetypes.ParseMemory(instance.Resources.Memory.Request)
		memLimBytes, _ := runetypes.ParseMemory(instance.Resources.Memory.Limit)

		// Apply CPU request as shares (soft)
		if cpuReqCores > 0 {
			shares := int64(cpuReqCores * 1024)
			if shares < 2 {
				shares = 2
			}
			hostConfig.Resources.CPUShares = shares
		}

		// Apply CPU limit (hard) via NanoCPUs when possible, else quota/period
		if cpuLimCores > 0 {
			// Prefer NanoCPUs (1e9 per core)
			hostConfig.Resources.NanoCPUs = int64(cpuLimCores * 1e9)
			if hostConfig.Resources.NanoCPUs == 0 {
				// Fallback to quota/period
				hostConfig.Resources.CPUPeriod = 100000
				hostConfig.Resources.CPUQuota = int64(cpuLimCores * float64(hostConfig.Resources.CPUPeriod))
			}
		}

		// Apply memory reservation (soft) and limit (hard)
		if memReqBytes > 0 {
			hostConfig.Resources.MemoryReservation = memReqBytes
		}
		if memLimBytes > 0 {
			hostConfig.Resources.Memory = memLimBytes
		}
	}

	// Handle secret and config mounts
	if instance.Metadata != nil {
		// Process secret mounts
		if len(instance.Metadata.SecretMounts) > 0 {
			secretMounts, err := r.prepareSecretMounts(instance.Metadata.SecretMounts)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to prepare secret mounts: %w", err)
			}
			hostConfig.Mounts = append(hostConfig.Mounts, secretMounts...)
		}

		// Process config mounts
		if len(instance.Metadata.ConfigmapMounts) > 0 {
			configMounts, err := r.prepareConfigmapsMounts(instance.Metadata.ConfigmapMounts)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to prepare config mounts: %w", err)
			}
			hostConfig.Mounts = append(hostConfig.Mounts, configMounts...)
		}

		// Process volume mounts. Source is the host-side path
		// produced by the storage driver (a managed directory, an
		// operator host path, or a block-device mount target). We only
		// emit bind mounts here; agent-side Attach/Mount/Unmount/Detach
		// for block-device drivers lives in the agent.
		if len(instance.Metadata.VolumeMounts) > 0 {
			for _, vm := range instance.Metadata.VolumeMounts {
				if vm.Source == "" || vm.MountPath == "" {
					return nil, nil, fmt.Errorf("invalid volume mount %q: source or mountPath empty", vm.Name)
				}
				source := vm.Source
				if vm.SubPath != "" {
					source = filepath.Join(source, vm.SubPath)
				}
				hostConfig.Mounts = append(hostConfig.Mounts, mount.Mount{
					Type:     mount.TypeBind,
					Source:   source,
					Target:   vm.MountPath,
					ReadOnly: vm.ReadOnly,
				})
			}
		}
	}

	// Inject the agent's embedded DNS (RUNE-063) when it has been
	// activated. Activation is gated by the agent on data-plane
	// readiness so containers never resolve names to unreachable
	// VIPs. --dns-search ensures bare names like "db" resolve to
	// "db.<namespace>.rune".
	r.dnsMu.RLock()
	dnsServers := append([]string(nil), r.dnsServers...)
	dnsSearch := append([]string(nil), r.dnsSearch...)
	r.dnsMu.RUnlock()
	if len(dnsServers) > 0 {
		hostConfig.DNS = append(hostConfig.DNS, dnsServers...)
		// Per-instance namespace search domain
		if instance.Namespace != "" {
			hostConfig.DNSSearch = append(hostConfig.DNSSearch, instance.Namespace+".rune")
		}
		hostConfig.DNSSearch = append(hostConfig.DNSSearch, dnsSearch...)
	}

	// Expose the service port inside the container so the edge
	// ingress controller can dial it over Docker networking. We
	// never publish a host port from the runner: when expose.host
	// is set, the ingress listener already owns :80/:443 on the
	// host and proxies in by container IP. Operators who want a
	// raw host bind for a non-ingress service can declare it via
	// the runner-level mechanism (TBD); the MVP host-bind escape
	// hatch (`expose.hostPort`) was removed in favor of a single
	// well-defined ingress path.
	if instance.Metadata != nil && instance.Metadata.Expose != nil && len(instance.Metadata.Ports) > 0 {
		// Resolve the referenced port by name or number
		var svcPort *runetypes.ServicePort
		ref := instance.Metadata.Expose.Port
		for i := range instance.Metadata.Ports {
			p := &instance.Metadata.Ports[i]
			if p.Name == ref {
				svcPort = p
				break
			}
			if ref != "" {
				if n, err := strconv.Atoi(ref); err == nil && n == p.Port {
					svcPort = p
					break
				}
			}
		}
		if svcPort != nil {
			proto := strings.ToLower(strings.TrimSpace(svcPort.Protocol))
			if proto == "" {
				proto = "tcp"
			}
			if proto == "tcp" {
				containerPort := nat.Port(fmt.Sprintf("%d/%s", svcPort.Port, proto))
				if containerConfig.ExposedPorts == nil {
					containerConfig.ExposedPorts = nat.PortSet{}
				}
				containerConfig.ExposedPorts[containerPort] = struct{}{}
				// No host-port publication; the ingress controller
				// reaches the container by container IP.
				instance.Metadata.ExposedHost = ""
				instance.Metadata.ExposedHostPort = 0
			}
		}
	}

	// Publish any port with hostPort > 0 to 127.0.0.1:<hostPort> on the
	// host. Dev-mode escape hatch for platforms where the cluster
	// dataplane cannot reach container bridge IPs from the host
	// (macOS Docker Desktop). Production services should reach each
	// other via the cluster VIP / ingress instead.
	if instance.Metadata != nil && len(instance.Metadata.Ports) > 0 {
		for _, p := range instance.Metadata.Ports {
			if p.HostPort <= 0 {
				continue
			}
			proto := strings.ToLower(strings.TrimSpace(p.Protocol))
			if proto == "" {
				proto = "tcp"
			}
			cport, err := nat.NewPort(proto, strconv.Itoa(p.Port))
			if err != nil {
				return nil, nil, fmt.Errorf("invalid container port %d/%s: %w", p.Port, proto, err)
			}
			if containerConfig.ExposedPorts == nil {
				containerConfig.ExposedPorts = nat.PortSet{}
			}
			containerConfig.ExposedPorts[cport] = struct{}{}
			if hostConfig.PortBindings == nil {
				hostConfig.PortBindings = nat.PortMap{}
			}
			hostConfig.PortBindings[cport] = append(hostConfig.PortBindings[cport], nat.PortBinding{
				HostIP:   "127.0.0.1",
				HostPort: strconv.Itoa(p.HostPort),
			})
		}
	}

	// Apply optional security context (seccomp / capabilities /
	// privileged). Privileged and seccomp=unconfined are gated
	// server-side; the runner only enforces structural correctness.
	applySecurityContext(hostConfig, instance.SecurityContext)

	return containerConfig, hostConfig, nil
}

// formatEnvVars formats a map of environment variables into a slice of "key=value" strings.
func formatEnvVars(env map[string]string) []string {
	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, fmt.Sprintf("%s=%s", k, v))
	}
	return result
}

// waitContainerIP polls the container inspect until its primary
// network reports an IP, the container has exited, or the budget
// expires. A single inspect issued right after ContainerStart can race
// the bridge IP assignment and return "", which left instances with an
// empty ContainerIP — breaking endpoint publishing and VIP routing.
func (r *DockerRunner) waitContainerIP(ctx context.Context, containerID string) string {
	const (
		budget   = 5 * time.Second
		interval = 100 * time.Millisecond
	)
	deadline := time.Now().Add(budget)
	for {
		if insp, err := r.client.ContainerInspect(ctx, containerID); err == nil {
			if insp.NetworkSettings != nil {
				if ip := pickContainerIP(insp.NetworkSettings); ip != "" {
					return ip
				}
			}
			// A container that has already exited will never get an IP.
			if insp.State != nil && !insp.State.Running {
				return ""
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(interval):
		}
	}
}

// InstanceIP implements runner.IPProvider for endpoint publishing.
func (r *DockerRunner) InstanceIP(ctx context.Context, instance *runetypes.Instance) (string, error) {
	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return "", err
	}
	insp, err := r.client.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}
	if ip := pickContainerIP(insp.NetworkSettings); ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("container has no IPv4 address")
}

// pickContainerIP returns the container's primary IPv4 address from
// an inspect result. Prefers the per-network EndpointSettings.IPAddress
// (works for user-defined networks too), and falls back to the legacy
// DefaultNetworkSettings.IPAddress for the default bridge.
func pickContainerIP(ns *container.NetworkSettings) string {
	if ns == nil {
		return ""
	}
	for _, ep := range ns.Networks {
		if ep != nil && ep.IPAddress != "" {
			return ep.IPAddress
		}
	}
	return ns.DefaultNetworkSettings.IPAddress
}

// prepareSecretMounts creates temporary files and Docker mounts for secret mounts
func (r *DockerRunner) prepareSecretMounts(secretMounts []types.ResolvedSecretMount) ([]mount.Mount, error) {
	var mounts []mount.Mount

	for _, secretMount := range secretMounts {
		// Create a temporary directory for this mount
		tempDir, err := os.MkdirTemp("", fmt.Sprintf("rune-secret-%s-", secretMount.Name))
		if err != nil {
			return nil, fmt.Errorf("failed to create temp directory for secret mount %s: %w", secretMount.Name, err)
		}
		// Adjust directory permissions to allow Docker Desktop to stat/bind mount
		// Keep files themselves locked down (0600) while directory is world-executable for traversal
		dirMode := r.config.SecretDirMode
		if dirMode == 0 {
			dirMode = 0o755
		}
		_ = os.Chmod(tempDir, dirMode)

		// Create files for each secret key
		for key, value := range secretMount.Data {
			// Determine the file path
			var filePath string
			if len(secretMount.Items) > 0 {
				// Check if there's a specific path mapping for this key
				for _, item := range secretMount.Items {
					if item.Key == key {
						filePath = filepath.Join(tempDir, item.Path)
						break
					}
				}
				// If no specific mapping, use the key name
				if filePath == "" {
					filePath = filepath.Join(tempDir, key)
				}
			} else {
				// No specific mapping, use the key name
				filePath = filepath.Join(tempDir, key)
			}

			// Ensure subdirectories exist if path contains directories
			// Use 0755 so the container user (often not the host owner due to Docker Desktop FUSE) can traverse
			parentMode := r.config.SecretDirMode
			if parentMode == 0 {
				parentMode = 0o755
			}
			if err := os.MkdirAll(filepath.Dir(filePath), parentMode); err != nil {
				os.RemoveAll(tempDir)
				return nil, fmt.Errorf("failed to create directory for secret file %s: %w", filePath, err)
			}
			// Create the file with the secret value (decode base64 if applicable)
			fileMode := r.config.SecretFileMode
			if fileMode == 0 {
				fileMode = 0o444
			}
			data := []byte(value)
			if decoded, ok := decodeIfBase64(value); ok {
				data = decoded
			}
			if err := os.WriteFile(filePath, data, fileMode); err != nil {
				os.RemoveAll(tempDir) // Clean up on error
				return nil, fmt.Errorf("failed to write secret file %s: %w", filePath, err)
			}
		}

		// Create Docker mount
		dockerMount := mount.Mount{
			Type:        mount.TypeBind,
			Source:      tempDir,
			Target:      secretMount.MountPath,
			ReadOnly:    true,
			BindOptions: &mount.BindOptions{},
		}

		mounts = append(mounts, dockerMount)
	}

	return mounts, nil
}

// decodeIfBase64 attempts to decode s as standard base64 if it looks like base64 content.
// Returns (decoded, true) when decoding is performed, otherwise (nil, false).
func decodeIfBase64(s string) ([]byte, bool) {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) == 0 || len(trimmed)%4 != 0 {
		return nil, false
	}
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			continue
		}
		return nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, false
	}
	return decoded, true
}

// prepareConfigmapsMounts creates temporary files and Docker mounts for config mounts
func (r *DockerRunner) prepareConfigmapsMounts(configMounts []types.ResolvedConfigmapMount) ([]mount.Mount, error) {
	var mounts []mount.Mount

	for _, configMount := range configMounts {
		// Create a temporary directory for this mount
		tempDir, err := os.MkdirTemp("", fmt.Sprintf("rune-config-%s-", configMount.Name))
		if err != nil {
			return nil, fmt.Errorf("failed to create temp directory for config mount %s: %w", configMount.Name, err)
		}
		// Adjust directory permissions to allow Docker Desktop to stat/bind mount
		dirMode := r.config.ConfigDirMode
		if dirMode == 0 {
			dirMode = 0o755
		}
		_ = os.Chmod(tempDir, dirMode)

		// Create files for each config key
		for key, value := range configMount.Data {
			// Determine the file path
			var filePath string
			if len(configMount.Items) > 0 {
				// Check if there's a specific path mapping for this key
				for _, item := range configMount.Items {
					if item.Key == key {
						filePath = filepath.Join(tempDir, item.Path)
						break
					}
				}
				// If no specific mapping, use the key name
				if filePath == "" {
					filePath = filepath.Join(tempDir, key)
				}
			} else {
				// No specific mapping, use the key name
				filePath = filepath.Join(tempDir, key)
			}

			// Ensure subdirectories exist if path contains directories
			// Use 0755 so the container user can traverse
			parentMode := r.config.ConfigDirMode
			if parentMode == 0 {
				parentMode = 0o755
			}
			if err := os.MkdirAll(filepath.Dir(filePath), parentMode); err != nil {
				os.RemoveAll(tempDir)
				return nil, fmt.Errorf("failed to create directory for config file %s: %w", filePath, err)
			}
			// Create the file with the config value
			fileMode := r.config.ConfigFileMode
			if fileMode == 0 {
				fileMode = 0o644
			}
			if err := os.WriteFile(filePath, []byte(value), fileMode); err != nil {
				os.RemoveAll(tempDir) // Clean up on error
				return nil, fmt.Errorf("failed to write config file %s: %w", filePath, err)
			}
		}

		// Create Docker mount
		dockerMount := mount.Mount{
			Type:        mount.TypeBind,
			Source:      tempDir,
			Target:      configMount.MountPath,
			ReadOnly:    true,
			BindOptions: &mount.BindOptions{},
		}

		mounts = append(mounts, dockerMount)
	}

	return mounts, nil
}
