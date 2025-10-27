package manager

import (
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
)

// IRunnerManager defines the interface for managing runners
type IRunnerManager interface {
	// Initialize initializes all runners
	Initialize() error

	// GetDockerRunner returns the Docker runner
	GetDockerRunner() (runner.Runner, error)

	// GetProcessRunner returns the Process runner
	GetProcessRunner() (runner.Runner, error)

	// GetInstanceRunner returns the appropriate runner for an instance
	GetInstanceRunner(instance *types.Instance) (runner.Runner, error)

	// GetServiceRunner returns the appropriate runner for a service
	GetServiceRunner(service *types.Service) (runner.Runner, error)

	// Close closes all runners
	Close() error
}
