// Package runner provides interfaces and implementations for managing service instances.
package runner

import (
	"context"
	"io"
	"time"

	"github.com/runestack/rune/pkg/types"
)

// Runner defines the interface for service runners, which are responsible for
// managing the lifecycle of service instances (containers, processes, etc.).
type Runner interface {
	// Type returns the type of runner.
	Type() types.RunnerType

	// Create creates a new service instance but does not start it.
	Create(ctx context.Context, instance *types.Instance) error

	// Start starts an existing service instance.
	Start(ctx context.Context, instance *types.Instance) error

	// Stop stops a running service instance.
	Stop(ctx context.Context, instance *types.Instance, timeout time.Duration) error

	// Remove removes a service instance.
	Remove(ctx context.Context, instance *types.Instance, force bool) error

	// GetLogs retrieves logs from a service instance.
	GetLogs(ctx context.Context, instance *types.Instance, options LogOptions) (io.ReadCloser, error)

	// Status retrieves the current status of a service instance.
	Status(ctx context.Context, instance *types.Instance) (types.InstanceStatus, error)

	// List lists all service instances managed by this runner.
	List(ctx context.Context, namespace string) ([]*types.Instance, error)

	// Exec creates an interactive exec session with a running instance.
	// Returns an ExecStream for bidirectional communication.
	Exec(ctx context.Context, instance *types.Instance, options ExecOptions) (ExecStream, error)
}

// RunnerProvider defines a simplified interface for getting runners
type RunnerProvider interface {
	// GetInstanceRunner returns the appropriate runner for an instance
	GetInstanceRunner(instance *types.Instance) (Runner, error)
}

// LogOptions defines options for retrieving logs.
type LogOptions struct {
	// Follow indicates whether to follow the log output (like tail -f).
	Follow bool

	// Tail indicates the number of lines to show from the end of the logs (0 for all).
	Tail int

	// Since shows logs since a specific timestamp.
	Since time.Time

	// Until shows logs until a specific timestamp.
	Until time.Time

	// Timestamps indicates whether to include timestamps.
	Timestamps bool
}

// InstanceStatus extends types.InstanceStatus with additional details needed by runners.
type InstanceStatus struct {
	// State is the current state of the instance.
	State types.InstanceStatus

	// ContainerID is the ID of the container (if applicable).
	ContainerID string

	// InstanceID is the ID of the Rune instance.
	InstanceID string

	// CreatedAt is when the instance was created.
	CreatedAt time.Time

	// StartedAt is when the instance was started.
	StartedAt time.Time

	// FinishedAt is when the instance finished or failed.
	FinishedAt time.Time

	// ExitCode is the exit code if the instance has stopped.
	ExitCode int

	// ErrorMessage contains any error information.
	ErrorMessage string
}

// ExecOptions defines options for executing a command in a running instance.
type ExecOptions struct {
	// Command is the command to execute.
	Command []string

	// Env is a map of environment variables to set for the command.
	Env map[string]string

	// WorkingDir is the working directory for the command.
	WorkingDir string

	// TTY indicates whether to allocate a pseudo-TTY.
	TTY bool

	// TerminalWidth is the initial width of the terminal.
	TerminalWidth uint32

	// TerminalHeight is the initial height of the terminal.
	TerminalHeight uint32
}

// ExecStream provides bidirectional communication with an exec session.
type ExecStream interface {
	// Write writes data to the standard input of the process.
	Write(p []byte) (n int, err error)

	// Read reads data from the standard output of the process.
	Read(p []byte) (n int, err error)

	// Stderr provides access to the standard error stream of the process.
	Stderr() io.Reader

	// ResizeTerminal resizes the terminal (if TTY was enabled).
	ResizeTerminal(width, height uint32) error

	// Signal sends a signal to the process.
	Signal(sigName string) error

	// ExitCode returns the exit code after the process has completed.
	// Returns an error if the process has not completed or if there was an error.
	ExitCode() (int, error)

	// Close terminates the exec session and releases resources.
	Close() error
}

// GetExecOptions returns ExecOptions using values from the instance
func GetExecOptions(command []string, instance *types.Instance) ExecOptions {
	opts := ExecOptions{
		Command: command,
		TTY:     false,
	}

	// Use instance environment if available
	if instance != nil && instance.Environment != nil {
		opts.Env = instance.Environment
	} else {
		opts.Env = make(map[string]string)
	}

	return opts
}
