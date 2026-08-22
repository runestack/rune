// Attach/read operations: status, logs, Exec, ExecDebug, Dial.
// Split from instance_controller.go (RUNE-311).

package instance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
)

// execStreamAdapter adapts runner.ExecStream to orchestrator.ExecStream
type execStreamAdapter struct {
	runner.ExecStream
}

// GetInstanceStatus gets the current status of an instance
func (c *Controller) GetInstanceStatus(ctx context.Context, instance *types.Instance) (*types.InstanceStatusInfo, error) {
	// For now, we'll try to get status from both runners and use the first one that succeeds
	runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get runner for instance: %w", err)
	}

	status, err := runner.Status(ctx, instance)
	if err == nil {
		// Assuming Status returns a string representing the state
		return &types.InstanceStatusInfo{
			Status:     status,
			InstanceID: instance.ID,
			NodeID:     instance.NodeID,
			CreatedAt:  instance.CreatedAt,
		}, nil
	}

	// If runner failed, return the instance status from the store
	return &types.InstanceStatusInfo{
		Status:     instance.Status,
		InstanceID: instance.ID,
		NodeID:     instance.NodeID,
		CreatedAt:  instance.CreatedAt,
	}, nil
}

// GetInstanceLogs gets logs for an instance. When the live container is
// unavailable (the runner has no record of it — usually because the
// instance has been tombstoned and the container removed, or because
// the host runner is down), fall back to the LastLogs snapshot
// captured at tombstone/retention-GC time. This is what makes
// `rune logs <failed-id>` and the service-level `rune logs <name>`
// keep working after the container is gone — operators investigating
// a failure do not have to race the retention GC.
func (c *Controller) GetInstanceLogs(ctx context.Context, instance *types.Instance, opts types.LogOptions) (io.ReadCloser, error) {
	_runner, runnerErr := c.runnerManager.GetInstanceRunner(instance)
	if runnerErr == nil {
		logs, err := _runner.GetLogs(ctx, instance, runner.LogOptions{
			Follow:     opts.Follow,
			Since:      opts.Since,
			Until:      opts.Until,
			Tail:       opts.Tail,
			Timestamps: opts.Timestamps,
		})
		if err == nil {
			// In follow mode we cannot peek (would block waiting for
			// the first byte that may never come for a silent
			// container). Hand the live stream through verbatim;
			// operators reaching for --follow are accepting that
			// "nothing right now" is a possible state.
			if opts.Follow {
				return logs, nil
			}
			// Non-follow: detect the silent-container case and prefer
			// the LastLogs snapshot from a previous attempt over a
			// zero-byte live stream. This is the load-bearing fix
			// for prod/gateway, where docker logs returned 0 bytes
			// for the current container while a prior attempt had
			// real crash output. Without this, `rune logs <id>`
			// against a silent container returns exit 0 + empty
			// body and the operator sees nothing.
			pr := NewPeekingReader(logs)
			if has, _ := pr.HasData(); has {
				return pr, nil
			}
			// Live reader was empty. Close it (we're abandoning it)
			// and fall through to LastLogs / synth path below.
			_ = pr.Close()
		}
		// Runner is reachable but the container is gone (err != nil)
		// or returned no data (handled above) — fall through to the
		// LastLogs snapshot below.
	}

	if len(instance.LastLogs) > 0 {
		c.logger.Debug("Serving LastLogs snapshot for instance",
			log.Str("instance", instance.ID),
			log.Int("bytes", len(instance.LastLogs)))
		return io.NopCloser(bytes.NewReader(instance.LastLogs)), nil
	}

	// Terminal-state instances with no captured stdout/stderr still
	// deserve SOMETHING from `rune logs` rather than silent empty
	// output. Synthesize a one-liner from the tombstone's
	// FailureReason / StatusMessage so operators can at least see
	// "instance died, here's why" instead of having to dig through
	// `rune get instance -o yaml` separately. Common case:
	// containers that crash before printing anything (PID 1
	// SIGKILL'd by a failed health check, image entrypoint exits
	// instantly, etc.).
	if isTerminalInstanceStatus(instance.Status) {
		return io.NopCloser(strings.NewReader(SynthesizeNoLogsLine(instance))), nil
	}

	if runnerErr != nil {
		return nil, fmt.Errorf("failed to get logs for instance %s: %w", instance.ID, runnerErr)
	}
	return nil, fmt.Errorf("failed to get logs for instance %s: container unavailable and no LastLogs snapshot", instance.ID)
}

// isTerminalInstanceStatus is true for statuses that mean the
// instance is not running and not coming back without operator
// action — so any "no logs" answer is final, not transient.
func isTerminalInstanceStatus(s types.InstanceStatus) bool {
	switch s {
	case types.InstanceStatusFailed,
		types.InstanceStatusStalled,
		types.InstanceStatusDeleted,
		types.InstanceStatusExited,
		types.InstanceStatusUnknown:
		return true
	}
	return false
}

// SynthesizeNoLogsLine builds a single user-facing line explaining
// why a terminal instance has nothing in its logs. Pulls everything
// from the tombstone record itself so the answer travels with
// `rune logs <id>` even after the container is gone.
func SynthesizeNoLogsLine(instance *types.Instance) string {
	var b strings.Builder
	b.WriteString("[rune] instance ")
	b.WriteString(instance.ID)
	b.WriteString(" (")
	b.WriteString(string(instance.Status))
	b.WriteString(") produced no captured output")
	if instance.FailureReason != "" {
		b.WriteString(" — reason: ")
		b.WriteString(instance.FailureReason)
	}
	if instance.StatusMessage != "" {
		b.WriteString("\n[rune] status: ")
		b.WriteString(instance.StatusMessage)
	}
	if instance.FailedAt != nil {
		b.WriteString("\n[rune] failed at: ")
		b.WriteString(instance.FailedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	b.WriteString("\n")
	return b.String()
}

// Exec executes a command in a running instance
// Dial opens a TCP connection to the given port on the instance's
// running container/process (RUNE-122).
func (c *Controller) Dial(ctx context.Context, instance *types.Instance, port uint32) (net.Conn, error) {
	c.logger.Debug("Dialing instance",
		log.Str("instance", instance.ID),
		log.Int("port", int(port)))

	if instance.Status != types.InstanceStatusRunning {
		return nil, fmt.Errorf("instance is not running, status: %s", instance.Status)
	}

	_runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get runner for instance: %w", err)
	}

	conn, err := _runner.Dial(ctx, instance, port)
	if err != nil {
		return nil, fmt.Errorf("failed to dial instance %s:%d: %w", instance.ID, port, err)
	}
	return conn, nil
}

func (c *Controller) Exec(ctx context.Context, instance *types.Instance, options types.ExecOptions) (types.ExecStream, error) {
	c.logger.Debug("Executing command in instance",
		log.Str("instance", instance.ID),
		log.Str("command", strings.Join(options.Command, " ")))

	// Get runner for the instance
	_runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get runner for instance: %w", err)
	}

	// Convert orchestrator exec options to runner exec options
	runnerOptions := runner.ExecOptions{
		Command:        options.Command,
		Env:            options.Env,
		WorkingDir:     options.WorkingDir,
		TTY:            options.TTY,
		TerminalWidth:  options.TerminalWidth,
		TerminalHeight: options.TerminalHeight,
	}

	// Create exec session with the runner
	execStream, err := _runner.Exec(ctx, instance, runnerOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to execute command in instance %s: %w", instance.ID, err)
	}

	return execStreamAdapter{execStream}, nil
}

// ExecDebug spawns an ephemeral inspection sidecar for the given (Failed)
// instance and execs options.Command inside it. The sidecar is removed when
// the returned ExecStream is Closed. Used by `rune exec --debug
// <tombstone-id>` to inspect the failed container's image+env+mounts state
// without re-running the failing app.
func (c *Controller) ExecDebug(ctx context.Context, instance *types.Instance, options types.ExecOptions) (types.ExecStream, error) {
	c.logger.Info("Spawning debug sidecar",
		log.Str("instance", instance.ID),
		log.Str("command", strings.Join(options.Command, " ")))

	_runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return nil, fmt.Errorf("failed to get runner for instance: %w", err)
	}

	runnerOptions := runner.ExecOptions{
		Command:        options.Command,
		Env:            options.Env,
		WorkingDir:     options.WorkingDir,
		TTY:            options.TTY,
		TerminalWidth:  options.TerminalWidth,
		TerminalHeight: options.TerminalHeight,
	}

	execStream, err := _runner.RunDebug(ctx, instance, runnerOptions)
	if err != nil {
		return nil, fmt.Errorf("debug exec on instance %s: %w", instance.ID, err)
	}
	return execStreamAdapter{execStream}, nil
}
