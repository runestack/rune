package manager

import (
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
)

// Ensure TestRunnerManager implements the IRunnerManager interface
var _ IRunnerManager = (*TestRunnerManager)(nil)

// TestRunnerManager is a mock implementation of IRunnerManager for testing purposes
type TestRunnerManager struct {
	// Test runners that will be returned by the respective getter methods
	MockDockerRunner  runner.Runner
	MockProcessRunner runner.Runner
	DefaultRunner     runner.Runner // Used when neither is specified

	// Track calls for testing assertions
	GetDockerCalled   bool
	GetProcessCalled  bool
	GetInstanceCalled bool
	GetServiceCalled  bool
	InitializeCalled  bool
	CloseCalled       bool
}

// Initialize implements the RunnerManager interface
func (m *TestRunnerManager) Initialize() error {
	m.InitializeCalled = true
	return nil
}

// GetDockerRunner implements the RunnerManager interface
func (m *TestRunnerManager) GetDockerRunner() (runner.Runner, error) {
	m.GetDockerCalled = true
	if m.MockDockerRunner != nil {
		return m.MockDockerRunner, nil
	}
	return m.DefaultRunner, nil
}

// GetProcessRunner implements the RunnerManager interface
func (m *TestRunnerManager) GetProcessRunner() (runner.Runner, error) {
	m.GetProcessCalled = true
	if m.MockProcessRunner != nil {
		return m.MockProcessRunner, nil
	}
	return m.DefaultRunner, nil
}

// GetInstanceRunner implements the RunnerManager interface
func (m *TestRunnerManager) GetInstanceRunner(instance *types.Instance) (runner.Runner, error) {
	m.GetInstanceCalled = true
	if instance.ContainerID != "" {
		return m.GetDockerRunner()
	}
	return m.GetProcessRunner()
}

// GetServiceRunner implements the RunnerManager interface
func (m *TestRunnerManager) GetServiceRunner(service *types.Service) (runner.Runner, error) {
	m.GetServiceCalled = true
	if service.Runtime == "container" {
		return m.GetDockerRunner()
	}
	return m.GetProcessRunner()
}

// Close implements the RunnerManager interface
func (m *TestRunnerManager) Close() error {
	m.CloseCalled = true
	return nil
}

// SetDockerRunner sets the Docker runner for testing
func (m *TestRunnerManager) SetDockerRunner(r runner.Runner) {
	m.MockDockerRunner = r
}

// SetProcessRunner sets the Process runner for testing
func (m *TestRunnerManager) SetProcessRunner(r runner.Runner) {
	m.MockProcessRunner = r
}

// NewTestRunnerManager creates a new test runner manager
func NewTestRunnerManager(defaultRunner runner.Runner) *TestRunnerManager {
	return &TestRunnerManager{
		DefaultRunner: defaultRunner,
	}
}
