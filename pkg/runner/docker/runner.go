// Package docker provides a Docker-based implementation of the runner interface.
package docker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
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

	// DisableAmbientRegistryAuth turns off the ambient fallbacks used
	// when no [[docker.registries]] entry matches an image host: the
	// GCE metadata service account (Artifact Registry / GCR) and the
	// docker CLI config of the user runed runs as. Off by default —
	// ambient auth only engages when explicit config resolved nothing.
	DisableAmbientRegistryAuth bool

	// Container log rotation for the json-file driver. Caps per-container log
	// growth on the host — the agent log forwarder reads container logs but does
	// not truncate them, so without this the daemon's logs grow unbounded.
	// LogMaxSize empty disables rotation (inherit the daemon default);
	// LogMaxFile <= 0 omits max-file.
	LogMaxSize string // e.g. "10m"
	LogMaxFile int    // e.g. 3
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
		// Cap per-container logs at 3×10m by default so the host doesn't fill up.
		LogMaxSize: "10m",
		LogMaxFile: 3,
	}
}

// logConfig returns the json-file log-rotation config applied to new containers.
// When LogMaxSize is empty an empty LogConfig is returned, so the container
// inherits the daemon's default logging (no rotation imposed by Rune).
func (c *DockerConfig) logConfig() container.LogConfig {
	if c == nil || c.LogMaxSize == "" {
		return container.LogConfig{}
	}
	opts := map[string]string{"max-size": c.LogMaxSize}
	if c.LogMaxFile > 0 {
		opts["max-file"] = strconv.Itoa(c.LogMaxFile)
	}
	return container.LogConfig{Type: "json-file", Config: opts}
}

// RegistryConfig defines a registry auth entry
type RegistryConfig struct {
	Name     string       `mapstructure:"name"`
	Registry string       `mapstructure:"registry"`
	Auth     RegistryAuth `mapstructure:"auth"`
}

// RegistryAuth defines supported auth types
type RegistryAuth struct {
	Type     string `mapstructure:"type"` // basic | token | dockerconfigjson | ecr | gcp
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	Token    string `mapstructure:"token"`
	Region   string `mapstructure:"region"`
}

// Validate that DockerRunner implements the runner.Runner interface
var _ runner.Runner = &DockerRunner{}

// Validate that DockerRunner implements the optional HealthChecker.
var _ runner.HealthChecker = &DockerRunner{}

// HealthCheck pings the Docker daemon. A non-nil error means the daemon is
// unreachable — the server keeps running and the store stays fine, but no
// container can be created/started/restarted until Docker recovers. This is
// the signal `rune status` surfaces as "Runners: docker=unreachable".
func (r *DockerRunner) HealthCheck(ctx context.Context) error {
	if r.client == nil {
		return fmt.Errorf("docker client not initialized")
	}
	_, err := r.client.Ping(ctx)
	return err
}

// DockerRunner implements the runner.Runner interface for Docker.
type DockerRunner struct {
	client *client.Client
	logger log.Logger
	config *DockerConfig

	// auth and mounts are the RUNE-312 Phase 2 collaborators: auth owns
	// registry-auth policy + its state, mounts materializes secret/config
	// mounts. auth is wired only here in the constructor (its sync.Once
	// provider chain must be shared); mounts is stateless and mounter()
	// derives one lazily for bare-struct tests.
	auth   *registryAuthResolver
	mounts *mountMaterializer

	// dnsMu guards dnsServers + dnsSearch which are toggled at
	// runtime by the agent once the data path (RUNE-041) is healthy
	// and the embedded DNS server (RUNE-063) has bound.
	dnsMu      sync.RWMutex
	dnsServers []string
	dnsSearch  []string

	// stats caches previous per-container CPU counters so InstanceStats
	// (runner.StatsProvider) can compute deltas across calls without a
	// blocking two-sample read every time. See stats.go.
	stats *statsCache

	// nodeID is this machine's identity, stamped onto the instances
	// List() reconstructs from container labels so they agree with the
	// stored records. Late-bound like dnsServers because the manager
	// builds its runners before runed hands over identity.
	nodeID atomic.Pointer[string]
}

func (r *DockerRunner) Type() types.RunnerType {
	return types.RunnerTypeDocker
}

// SetNodeID wires this machine's node identity. Until it is called,
// NodeID() reports types.LocalNodeIDFallback.
func (r *DockerRunner) SetNodeID(nodeID string) {
	r.nodeID.Store(&nodeID)
}

// NodeID returns the wired node identity, or types.LocalNodeIDFallback.
func (r *DockerRunner) NodeID() string {
	if p := r.nodeID.Load(); p != nil && *p != "" {
		return *p
	}
	return types.LocalNodeIDFallback
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
		client: client,
		logger: logger,
		config: config,
		// The resolver's provider chain is still built on first pull,
		// inside registryAuthResolver.registryProviders.
		auth:   newRegistryAuthResolver(config, logger),
		mounts: newMountMaterializer(config),
		stats:  newStatsCache(),
	}, nil
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
