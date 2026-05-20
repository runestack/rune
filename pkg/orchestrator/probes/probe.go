package probes

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
)

// ProbeResult represents the result of a health check probe
type ProbeResult struct {
	Success  bool
	Message  string
	Duration time.Duration
}

// ProbeContext contains all context needed to execute a probe
type ProbeContext struct {
	// Context for cancellation and timeouts
	Ctx context.Context

	// Logger
	Logger log.Logger

	// Instance being checked
	Instance *types.Instance

	// Probe configuration
	ProbeConfig *types.Probe

	// HTTP client for HTTP probes
	HTTPClient *http.Client

	// Runner provider for exec probes
	RunnerProvider runner.RunnerProvider
}

// Prober defines the interface for health check probes
type Prober interface {
	// Execute runs the probe and returns the result
	Execute(ctx *ProbeContext) ProbeResult
}

// probeHost returns the address to dial for network probes against
// the given instance. Container instances expose a per-container IP
// on the Docker bridge (recorded by the runner on Start as
// Metadata.ContainerIP); we must dial that directly so the probe
// hits the container instead of the host's loopback. Process
// instances run on the host, so localhost is correct.
//
// Probing through localhost on edge nodes would otherwise hit the
// runed ingress listener on :80 / :443 and 404 (no Host header
// match), causing healthy containers to look unhealthy and
// restart-loop forever.
func probeHost(instance *types.Instance) string {
	if instance != nil && instance.Metadata != nil && instance.Metadata.ContainerIP != "" {
		return instance.Metadata.ContainerIP
	}
	return "localhost"
}

// ExecProber implements the Exec health check probe
type ExecProber struct{}

// Execute implements the Prober interface for Exec probes
func (p *ExecProber) Execute(ctx *ProbeContext) ProbeResult {
	start := time.Now()

	// Check if command is specified
	if len(ctx.ProbeConfig.Command) == 0 {
		return ProbeResult{
			Success:  false,
			Message:  "Exec health check failed: no command specified",
			Duration: time.Since(start),
		}
	}

	// Get runner for the instance
	_runner, err := ctx.RunnerProvider.GetInstanceRunner(ctx.Instance)
	if err != nil {
		return ProbeResult{
			Success:  false,
			Message:  fmt.Sprintf("Exec health check failed to get runner: %v", err),
			Duration: time.Since(start),
		}
	}

	// Bound the exec to the probe timeout. ExitCode() waits for the
	// command to finish; without a deadline a slow-starting command
	// (e.g. mongosh, a Node.js binary that needs ~1-2s to boot) would
	// otherwise be checked once, found "still running", and scored a
	// false failure. The context also caps ExitCode()'s wait loop.
	timeout := time.Duration(ctx.ProbeConfig.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	execCtx, cancel := context.WithTimeout(ctx.Ctx, timeout)
	defer cancel()

	// Execute the command
	execOpts := runner.GetExecOptions(ctx.ProbeConfig.Command, ctx.Instance)
	execStream, err := _runner.Exec(execCtx, ctx.Instance, execOpts)
	if err != nil {
		return ProbeResult{
			Success:  false,
			Message:  fmt.Sprintf("Exec health check failed to start command: %v", err),
			Duration: time.Since(start),
		}
	}
	defer execStream.Close()

	// Wait for command completion and check exit code
	exitCode, err := execStream.ExitCode()
	if err != nil {
		return ProbeResult{
			Success:  false,
			Message:  fmt.Sprintf("Exec health check failed to get exit code: %v", err),
			Duration: time.Since(start),
		}
	}

	if exitCode == 0 {
		return ProbeResult{
			Success:  true,
			Message:  "Exec health check succeeded with exit code 0",
			Duration: time.Since(start),
		}
	}

	return ProbeResult{
		Success:  false,
		Message:  fmt.Sprintf("Exec health check failed with exit code %d", exitCode),
		Duration: time.Since(start),
	}
}

// NewProber creates a new probe implementation based on probe type
func NewProber(probeType string) (Prober, error) {
	switch probeType {
	case "http":
		return &HTTPProber{}, nil
	case "tcp":
		return &TCPProber{}, nil
	case "exec":
		return &ExecProber{}, nil
	default:
		return nil, fmt.Errorf("unknown probe type: %s", probeType)
	}
}
