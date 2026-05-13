// Package process — RUNE-121 init-step execution for the process runtime.
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner/process/security"
	"github.com/runestack/rune/pkg/types"
)

// initLogTailLimit caps how much of the step's combined stdout/stderr
// the runner buffers into the structured log. Init steps that need
// long-form logging will get a real log subsystem in S6 (RUNE-121).
const initLogTailLimit = 32 * 1024

// RunInit implements runner.Runner.RunInit for the process runtime.
//
// Process init steps run as a synchronous child of the runner. Unlike
// the docker runner, there is no image to pull and no bind-mount setup:
// the parent service's volumes are already part of the host filesystem
// (the process runtime treats type=hostPath as the only supported volume
// kind), and secret/configmap mounts have already been materialised onto
// disk by Create(). The step.Volumes / step.SecretMounts /
// step.ConfigmapMounts filters are therefore advisory in this runtime —
// the controller honours them at evaluation time but RunInit does not
// re-validate them.
//
// On normal termination the container's exit code is returned (zero on
// success). On runtime errors (fork/exec failure, security context
// failure, etc.) the error is non-nil and the exit code is undefined.
//
// Cancellation of ctx kills the step process group.
func (r *ProcessRunner) RunInit(ctx context.Context, instance *types.Instance, step types.InitStep) (int, error) {
	if instance == nil {
		return 0, fmt.Errorf("invalid instance: nil pointer")
	}
	if step.Name == "" {
		return 0, fmt.Errorf("invalid init step: name is empty")
	}
	if step.Command == "" {
		return 0, fmt.Errorf("invalid init step %q: command is required", step.Name)
	}
	if step.Image != "" {
		return 0, fmt.Errorf("invalid init step %q: image is not supported by the process runtime", step.Name)
	}

	// Working directory: prefer the parent's working dir so the step
	// sees the same view of the filesystem as the long-running service.
	// Fall back to the per-instance workspace under baseDir.
	workDir := filepath.Join(r.baseDir, "rune", instance.Namespace, instance.ID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return 0, fmt.Errorf("init step %q: failed to ensure workspace dir: %w", step.Name, err)
	}
	if instance.Process != nil && instance.Process.WorkingDir != "" {
		workDir = instance.Process.WorkingDir
	}

	cmd := exec.CommandContext(ctx, step.Command, step.Args...)
	cmd.Dir = workDir

	// Env: parent first, step overlays. Always inherit the runner's
	// own environment so the step can find PATH / TMPDIR / etc.
	env := os.Environ()
	for k, v := range instance.Environment {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range step.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = env

	// Inherit the parent service's security context if set. Init steps
	// that need to e.g. format a volume owned by the service user must
	// run as that user.
	if instance.Process != nil && instance.Process.SecurityContext != nil {
		if err := security.ApplySecurityContext(cmd, instance.Process.SecurityContext, r.logger); err != nil {
			return 0, fmt.Errorf("init step %q: failed to apply security context: %w", step.Name, err)
		}
	}

	// Capture combined output into a bounded ring so we can surface a
	// log tail through structured logging. S6 will route this to the
	// log subsystem so `rune logs svc/<step>` returns it.
	tail := &tailBuffer{max: initLogTailLimit}
	cmd.Stdout = tail
	cmd.Stderr = tail

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("init step %q: failed to start: %w", step.Name, err)
	}

	// Apply resource limits. Step resources override instance
	// resources when set. cgroups v2 is Linux-only; on other platforms
	// or unprivileged setups newResourceController returns an error
	// which we log and continue.
	res := instance.Resources
	if step.Resources != nil {
		res = step.Resources
	}
	if res != nil && cmd.Process != nil {
		if rc, rcErr := newResourceController(cmd.Process.Pid, res); rcErr != nil {
			r.logger.Warn("Init step resource limits not applied",
				log.Str("instance_id", instance.ID),
				log.Str("step", step.Name),
				log.Err(rcErr))
		} else {
			defer func() {
				if cleanupErr := rc.cleanup(); cleanupErr != nil {
					r.logger.Warn("Failed to clean up init step cgroup",
						log.Str("instance_id", instance.ID),
						log.Str("step", step.Name),
						log.Err(cleanupErr))
				}
			}()
		}
	}

	waitErr := cmd.Wait()
	// If the context fired during the wait (cancellation or deadline),
	// surface it as a runtime error so the controller can distinguish
	// it from a clean non-zero exit. exec.CommandContext kills the
	// process with SIGKILL on cancellation which would otherwise show
	// up as exit code -1 here.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return 0, fmt.Errorf("init step %q: cancelled: %w", step.Name, ctxErr)
	}
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				exitCode = status.ExitStatus()
			} else {
				exitCode = exitErr.ExitCode()
			}
		} else {
			// Not a normal process exit (cancelled, fork error after
			// start, etc.). Surface as a runtime error so the
			// controller can distinguish it from a non-zero exit.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, fmt.Errorf("init step %q: cancelled: %w", step.Name, ctxErr)
			}
			return 0, fmt.Errorf("init step %q: wait failed: %w", step.Name, waitErr)
		}
	}

	logFields := []log.Field{
		log.Str("instance_id", instance.ID),
		log.Str("instance_name", instance.Name),
		log.Str("step", step.Name),
		log.Int("exit_code", exitCode),
		log.Duration("duration", time.Since(timeOf(cmd))),
	}
	if tail.Len() > 0 {
		logFields = append(logFields, log.Str("output_tail", tail.String()))
	}
	r.logger.Info("Init step completed", logFields...)

	return exitCode, nil
}

// tailBuffer is a bytes.Buffer that drops oldest data once it reaches
// max bytes. We use it to keep the most recent N bytes of init step
// output for diagnostics without blowing memory on a noisy step.
type tailBuffer struct {
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n, err := t.buf.Write(p)
	if t.buf.Len() > t.max {
		excess := t.buf.Len() - t.max
		t.buf.Next(excess)
	}
	return n, err
}

func (t *tailBuffer) Len() int       { return t.buf.Len() }
func (t *tailBuffer) String() string { return t.buf.String() }
func (t *tailBuffer) Bytes() []byte  { return t.buf.Bytes() }
func (t *tailBuffer) Reset()         { t.buf.Reset() }

// timeOf returns the start time of the underlying os.Process if
// available, else now (so the duration field is at worst ~0s).
func timeOf(cmd *exec.Cmd) time.Time {
	if cmd == nil || cmd.ProcessState == nil {
		return time.Now()
	}
	// ProcessState.UserTime / SystemTime are durations, not start
	// timestamps; Go's exec API does not expose start time directly.
	// We approximate by subtracting elapsed CPU time from now, which
	// is good enough for a structured log field.
	return time.Now().Add(-cmd.ProcessState.UserTime() - cmd.ProcessState.SystemTime())
}
