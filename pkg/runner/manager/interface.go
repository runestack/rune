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

	// SetDNSInjection configures the cluster DNS servers + search
	// domains injected into every subsequently-created container.
	// Containers without injection inherit the host's /etc/resolv.conf
	// and therefore cannot resolve `<service>.<namespace>.rune` —
	// the entire Rune service-discovery layer hinges on this call
	// firing once after the agent's embedded DNS subsystem is up.
	// No-op on runners that don't support DNS injection (process
	// runner).
	SetDNSInjection(servers []string, search []string)

	// SetNodeID wires this machine's node identity so instances the
	// runner reconstructs from container labels carry the same node ID
	// as the stored records. No-op on runners that don't reconstruct
	// instances.
	SetNodeID(nodeID string)

	// Close closes all runners
	Close() error
}
