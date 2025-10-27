package manager

import (
	"sync"

	"os"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/docker"
	"github.com/runestack/rune/pkg/runner/process"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"github.com/spf13/viper"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Error is a simple error type
type Error string

func (e Error) Error() string { return string(e) }

// Define errors
var (
	ErrNotInitialized            = Error("runner manager not initialized")
	ErrDockerRunnerNotAvailable  = Error("docker runner not available")
	ErrProcessRunnerNotAvailable = Error("process runner not available")
)

// RunnerManager provides centralized access to various runners
type RunnerManager struct {
	dockerRunner  runner.Runner
	processRunner runner.Runner
	logger        log.Logger
	mutex         sync.RWMutex
	initialized   bool
}

// RunnerManagerOptions holds configuration for the runner manager
type RunnerManagerOptions struct {
	Logger       log.Logger
	DockerConfig *docker.DockerConfig
}

// NewRunnerManager creates a new runner manager
func NewRunnerManager(logger log.Logger) *RunnerManager {
	return &RunnerManager{
		logger: logger.WithComponent("runner-manager"),
	}
}

// getDockerConfig loads Docker configuration from Viper, which consolidates
// settings from config files, environment variables, and flags
func getDockerConfig() *docker.DockerConfig {
	config := docker.DefaultDockerConfig()

	// In Viper, environment variables are automatically bound with a prefix
	// and proper naming conventions, so we don't need separate env handling

	// Load configuration values with precedence already handled by Viper
	if viper.IsSet("docker.api_version") {
		config.APIVersion = viper.GetString("docker.api_version")
	}

	if viper.IsSet("docker.fallback_api_version") {
		config.FallbackAPIVersion = viper.GetString("docker.fallback_api_version")
	}

	if viper.IsSet("docker.negotiation_timeout_seconds") {
		config.NegotiationTimeoutSeconds = viper.GetInt("docker.negotiation_timeout_seconds")
	}

	// Optional mount permission settings
	if viper.IsSet("docker.secret_dir_mode") {
		config.SecretDirMode = os.FileMode(utils.ToUint32NonNegative(viper.GetInt("docker.secret_dir_mode")))
	}
	if viper.IsSet("docker.secret_file_mode") {
		config.SecretFileMode = os.FileMode(utils.ToUint32NonNegative(viper.GetInt("docker.secret_file_mode")))
	}
	if viper.IsSet("docker.config_dir_mode") {
		config.ConfigDirMode = os.FileMode(utils.ToUint32NonNegative(viper.GetInt("docker.config_dir_mode")))
	}
	if viper.IsSet("docker.config_file_mode") {
		config.ConfigFileMode = os.FileMode(utils.ToUint32NonNegative(viper.GetInt("docker.config_file_mode")))
	}

	// Load registry auth entries from viper if present
	if viper.IsSet("docker.registries") {
		var regs []map[string]any
		if err := viper.UnmarshalKey("docker.registries", &regs); err == nil {
			for _, r := range regs {
				rc := docker.RegistryConfig{}
				if name, ok := r["name"].(string); ok {
					rc.Name = name
				}
				if reg, ok := r["registry"].(string); ok {
					rc.Registry = reg
				}
				if authRaw, ok := r["auth"].(map[string]any); ok {
					if t, ok := authRaw["type"].(string); ok {
						rc.Auth.Type = t
					}
					if u, ok := authRaw["username"].(string); ok {
						rc.Auth.Username = u
					}
					if p, ok := authRaw["password"].(string); ok {
						rc.Auth.Password = p
					}
					if tk, ok := authRaw["token"].(string); ok {
						rc.Auth.Token = tk
					}
					if rg, ok := authRaw["region"].(string); ok {
						rc.Auth.Region = rg
					}
				}
				config.Registries = append(config.Registries, rc)
			}
		}
	}

	return config
}

// Initialize initializes all runners
func (m *RunnerManager) Initialize() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if m.initialized {
		return nil
	}

	// Get Docker configuration via Viper (which already handles env vars and config files)
	dockerConfig := getDockerConfig()

	// Debug: show loaded registry auth entries (no secrets)
	if len(dockerConfig.Registries) > 0 {
		m.logger.Debug("Loaded Docker registries from config",
			log.Int("count", len(dockerConfig.Registries)))
		for _, rc := range dockerConfig.Registries {
			m.logger.Debug("Registry entry",
				log.Str("name", rc.Name),
				log.Str("registry", rc.Registry),
				log.Str("type", rc.Auth.Type),
				log.Str("region", rc.Auth.Region))
		}
	} else {
		m.logger.Debug("No Docker registries configured")
	}

	// Create Docker runner with configuration
	dockerRunner, err := docker.NewDockerRunnerWithConfig(
		m.logger.WithComponent("docker-runner"),
		dockerConfig,
	)
	if err != nil {
		m.logger.Warn("Failed to initialize Docker runner", log.Err(err))
	} else {
		m.dockerRunner = dockerRunner
		m.logger.Info("Docker runner initialized",
			log.Str("api_version", dockerConfig.APIVersion),
			log.Str("fallback_version", dockerConfig.FallbackAPIVersion),
			log.Int("timeout_seconds", dockerConfig.NegotiationTimeoutSeconds))
	}

	// Create Process runner
	processRunner, err := process.NewProcessRunner(
		process.WithLogger(m.logger.WithComponent("process-runner")),
	)
	if err != nil {
		m.logger.Warn("Failed to initialize Process runner", log.Err(err))
	} else {
		m.processRunner = processRunner
	}

	m.initialized = true
	return nil
}

// GetDockerRunner returns the Docker runner
func (m *RunnerManager) GetDockerRunner() (runner.Runner, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.initialized {
		return nil, ErrNotInitialized
	}

	if m.dockerRunner == nil {
		return nil, ErrDockerRunnerNotAvailable
	}

	return m.dockerRunner, nil
}

// GetProcessRunner returns the Process runner
func (m *RunnerManager) GetProcessRunner() (runner.Runner, error) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if !m.initialized {
		return nil, ErrNotInitialized
	}

	if m.processRunner == nil {
		return nil, ErrProcessRunnerNotAvailable
	}

	return m.processRunner, nil
}

// Close closes all runners
func (m *RunnerManager) Close() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Close runners if they implement a Close method
	if closer, ok := m.dockerRunner.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			m.logger.Warn("Error closing Docker runner", log.Err(err))
		}
	}

	if closer, ok := m.processRunner.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			m.logger.Warn("Error closing Process runner", log.Err(err))
		}
	}

	m.initialized = false
	return nil
}

// GetInstanceRunner returns the appropriate runner to use for the given instance.
func (m *RunnerManager) GetInstanceRunner(instance *types.Instance) (runner.Runner, error) {
	switch instance.Runner {
	case types.RunnerTypeDocker:
		dockerRunner, err := m.GetDockerRunner()
		if err != nil {
			return nil, status.Error(codes.Unavailable, "docker runner not available")
		}
		return dockerRunner, nil

	case types.RunnerTypeProcess:
		processRunner, err := m.GetProcessRunner()
		if err != nil {
			return nil, status.Error(codes.Unavailable, "process runner not available")
		}
		return processRunner, nil

	default:
		// Fallback to existing heuristic for backward compatibility
		if instance.ContainerID != "" {
			dockerRunner, err := m.GetDockerRunner()
			if err != nil {
				return nil, status.Error(codes.Unavailable, "docker runner not available")
			}
			return dockerRunner, nil
		} else {
			processRunner, err := m.GetProcessRunner()
			if err != nil {
				return nil, status.Error(codes.Unavailable, "process runner not available")
			}
			return processRunner, nil
		}
	}
}

// GetServiceRunner returns the appropriate runner to use for the given service.
func (m *RunnerManager) GetServiceRunner(service *types.Service) (runner.Runner, error) {
	if service.Runtime == types.RuntimeTypeProcess {
		processRunner, err := m.GetProcessRunner()
		if err != nil {
			return nil, status.Error(codes.Unavailable, "process runner not available")
		}
		return processRunner, nil
	}

	dockerRunner, err := m.GetDockerRunner()
	if err != nil {
		return nil, status.Error(codes.Unavailable, "docker runner not available")
	}

	return dockerRunner, nil
}

/*
// SetDockerRunner sets a custom Docker runner (for testing)
func (m *RunnerManager) SetDockerRunner(runner runner.Runner) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.dockerRunner = runner
	if !m.initialized {
		m.initialized = true
	}
}

// SetProcessRunner sets a custom Process runner (for testing)
func (m *RunnerManager) SetProcessRunner(runner runner.Runner) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.processRunner = runner
	if !m.initialized {
		m.initialized = true
	}
}*/
