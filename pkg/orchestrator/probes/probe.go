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

	// Execute the command
	execOpts := runner.GetExecOptions(ctx.ProbeConfig.Command, ctx.Instance)
	execStream, err := _runner.Exec(ctx.Ctx, ctx.Instance, execOpts)
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
