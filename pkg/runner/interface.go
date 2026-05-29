// Package runner provides interfaces and implementations for managing service instances.
package runner

import (
	"context"
	"io"
	"net"
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

	// RunDebug spawns an ephemeral inspection container ("sidecar") from
	// the given (typically Failed) instance's image+env+mounts, with the
	// entrypoint overridden to `sleep infinity` so the inner app does NOT
	// re-run, then opens an exec session against the sidecar to run the
	// caller's command. The sidecar is stopped and removed when the
	// returned ExecStream is Closed (use defer). The original instance's
	// container is never touched.
	//
	// Runners that don't support spawn-from-template return
	// ErrDebugNotSupported.
	RunDebug(ctx context.Context, instance *types.Instance, options ExecOptions) (ExecStream, error)

	// Dial opens a TCP connection to the given port on the running
	// instance. Used by the port-forward command (RUNE-122). The
	// returned net.Conn is owned by the caller and must be Closed.
	// Implementations should honour ctx for the dial itself; the
	// returned Conn carries its own lifetime thereafter.
	Dial(ctx context.Context, instance *types.Instance, port uint32) (net.Conn, error)

	// RunInit executes one InitStep (RUNE-121) synchronously against
	// the given instance and returns the step's exit code (0 on
	// success). The runner is responsible for:
	//
	//   - filtering the instance's resolved volume / secret / config
	//     mounts according to step.Volumes / step.SecretMounts /
	//     step.ConfigmapMounts (a nil filter inherits all; a non-nil
	//     empty filter mounts none),
	//   - merging step.Env over the instance environment (step keys
	//     win),
	//   - applying step.Resources when set (otherwise inherit the
	//     instance's resources),
	//   - honouring the instance's image-pull policy for step.Image,
	//   - waiting for the step to terminate (or be cancelled via
	//     ctx) and capturing logs,
	//   - cleaning up the underlying container/process before
	//     returning.
	//
	// The instance controller, not the runner, owns iteration order,
	// runIf evaluation, restart-policy retries, and persistence of
	// InitStepState.
	//
	// Runners that do not support init steps must return
	// ErrInitNotSupported.
	RunInit(ctx context.Context, instance *types.Instance, step types.InitStep) (exitCode int, err error)
}

// IPProvider is implemented by runners that can resolve a running
// instance's primary routable IP for endpoint publishing.
type IPProvider interface {
	InstanceIP(ctx context.Context, instance *types.Instance) (string, error)
}

// HealthChecker is implemented by runners that can verify their backing
// runtime is reachable (e.g. the Docker runner pinging the daemon). A
// runner that does not implement it is treated as always-ready — correct
// for the process runner, which has no external daemon. Used by
// RunnerManager.RunnerHealth to surface "Docker is down" in `rune status`.
type HealthChecker interface {
	HealthCheck(ctx context.Context) error
}

// ErrInitNotSupported is returned by Runner.RunInit implementations
// that do not (yet) support init steps. The instance controller
// surfaces this as a fatal scheduling error so operators get a clear
// message rather than a silent skip.
var ErrInitNotSupported = errInitNotSupported{}

type errInitNotSupported struct{}

func (errInitNotSupported) Error() string {
	return "runner does not support init steps"
}

// ErrDebugNotSupported is returned by Runner.RunDebug implementations
// that don't support spawning an ephemeral sidecar from an existing
// instance's template (process runner, test runner). The exec service
// surfaces this as FailedPrecondition with a clear "your runtime
// doesn't support --debug" message instead of pretending it worked.
var ErrDebugNotSupported = errDebugNotSupported{}

type errDebugNotSupported struct{}

func (errDebugNotSupported) Error() string {
	return "runner does not support --debug inspection sidecars"
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
