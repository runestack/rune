// Package instance — RUNE-121 init-step orchestration for the
// instance controller. Owns iteration order, runIf evaluation,
// per-step retry policy, and persistence of InitStepState. Runners
// only execute one step at a time via Runner.RunInit.
package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
)

// initStepDefaultTimeout is used when a step does not declare its own.
var initStepDefaultTimeout = 5 * time.Minute

// initStepOnFailureMaxAttempts caps the controller-side retry loop for
// steps with restartPolicy=OnFailure. The first attempt counts.
var initStepOnFailureMaxAttempts = 3

// initStepOnFailureBackoff is the initial backoff between attempts.
// Exponential: 1s, 2s, 4s.
var initStepOnFailureBackoff = 1 * time.Second

// runInitSteps executes service.InitSteps against instance, in
// declaration order, before the main container is created. On any
// terminal failure it sets instance.Status=Failed (with a descriptive
// message) and returns a non-nil error so the caller bails out.
//
// Side effects:
//   - moves instance.Status to Initializing for the duration
//   - persists Instance.InitStates after every step transition
//   - never starts the main container; that is the caller's job
//
// runIf evaluation:
//   - always       → always run
//   - fileMissing  → stat the path against parent volume host paths;
//     run iff absent in every matching mount
//   - freshVolume  → run iff no prior Succeeded state exists for this
//     step on this instance. (S4 will switch the anchor to
//     Volume.Status.InitializedFor for proper cross-restart semantics.)
func (c *Controller) runInitSteps(ctx context.Context, serviceRunner runner.Runner, service *types.Service, instance *types.Instance) error {
	if len(service.InitSteps) == 0 {
		return nil
	}

	serviceKey := initStepServiceKey(service)

	// Move to Initializing so watchers (cast --wait, get service) can
	// surface the new phase.
	instance.Status = types.InstanceStatusInitializing
	instance.StatusMessage = "Running init steps"
	if uerr := c.store.Update(ctx, types.ResourceTypeInstance, service.Namespace, instance.ID, instance); uerr != nil {
		c.logger.Error("Failed to mark instance Initializing",
			log.Str("instance", instance.ID),
			log.Err(uerr))
	}

	// Pre-allocate one row per declared step so observers see the full
	// plan immediately (Pending entries become Succeeded/Failed/Skipped
	// as we progress).
	instance.InitStates = make([]types.InitStepState, 0, len(service.InitSteps))
	for _, step := range service.InitSteps {
		instance.InitStates = append(instance.InitStates, types.InitStepState{
			Name:   step.Name,
			Status: types.InitStepStatusPending,
		})
	}
	c.persistInitStates(ctx, instance)

	for i := range service.InitSteps {
		step := service.InitSteps[i]
		state := &instance.InitStates[i]

		shouldRun, skipReason := c.evaluateRunIf(ctx, step, instance, serviceKey)
		if !shouldRun {
			state.Status = types.InitStepStatusSkipped
			state.Message = skipReason
			c.persistInitStates(ctx, instance)
			c.logger.Info("Init step skipped",
				log.Str("instance", instance.ID),
				log.Str("step", step.Name),
				log.Str("reason", skipReason))
			continue
		}

		if err := c.executeInitStep(ctx, serviceRunner, instance, step, state); err != nil {
			// Terminal failure: mark instance Failed and propagate.
			instance.Status = types.InstanceStatusFailed
			instance.StatusMessage = fmt.Sprintf("init step %q: %s", step.Name, state.Message)
			if uerr := c.store.Update(ctx, types.ResourceTypeInstance, service.Namespace, instance.ID, instance); uerr != nil {
				c.logger.Error("Failed to mark instance Failed after init step",
					log.Str("instance", instance.ID),
					log.Err(uerr))
			}
			return fmt.Errorf("init step %q failed: %w", step.Name, err)
		}

		// On success, anchor freshness on the parent volumes so that
		// crash-recovery and restart correctly skip the step. Failures
		// here are logged but do not bubble: the next init pass will see
		// the missing flag and re-run, which is the safer of the two
		// failure modes (re-running an idempotent format script vs
		// silently corrupting an already-formatted volume).
		c.markVolumesInitialized(ctx, instance, step, serviceKey)
	}

	return nil
}

// initStepServiceKey is the canonical key used in
// Volume.InitializedFor for a given Service. Format: "<namespace>/<id>"
// — the ID rather than the name so that re-creating a service under
// the same name after a delete does not falsely skip its init steps
// against an old volume that happened to be reclaim-retained.
func initStepServiceKey(svc *types.Service) string {
	return svc.Namespace + "/" + svc.ID
}

// markVolumesInitialized stamps every parent volume permitted by the
// step's Volumes filter with InitializedFor[serviceKey]=now. Volumes
// that fail to load or update are logged and skipped.
func (c *Controller) markVolumesInitialized(
	ctx context.Context,
	instance *types.Instance,
	step types.InitStep,
	serviceKey string,
) {
	mounts := selectInitVolumes(instance, step, "")
	now := time.Now().UTC()
	for _, m := range mounts {
		if m.VolumeName == "" {
			continue // host-only mount, no Volume resource to anchor on
		}
		ns := m.VolumeNamespace
		if ns == "" {
			ns = instance.Namespace
		}
		var vol types.Volume
		if err := c.store.Get(ctx, types.ResourceTypeVolume, ns, m.VolumeName, &vol); err != nil {
			c.logger.Warn("Init step succeeded but failed to load parent volume for InitializedFor",
				log.Str("volume", ns+"/"+m.VolumeName),
				log.Str("step", step.Name),
				log.Err(err))
			continue
		}
		if vol.InitializedFor == nil {
			vol.InitializedFor = make(map[string]time.Time, 1)
		}
		vol.InitializedFor[serviceKey] = now
		if err := c.store.Update(ctx, types.ResourceTypeVolume, ns, vol.Name, &vol); err != nil {
			c.logger.Warn("Failed to persist Volume.InitializedFor",
				log.Str("volume", ns+"/"+m.VolumeName),
				log.Str("step", step.Name),
				log.Err(err))
		}
	}
}

// executeInitStep runs a single step honouring its restart policy and
// timeout. It updates state in place and persists after each attempt.
// Returns nil on success; non-nil only when the step has exhausted its
// retries or hit a non-retryable failure (Never policy).
func (c *Controller) executeInitStep(
	ctx context.Context,
	r runner.Runner,
	instance *types.Instance,
	step types.InitStep,
	state *types.InitStepState,
) error {
	maxAttempts := 1
	if step.RestartPolicy == types.InitStepRestartOnFailure || step.RestartPolicy == "" {
		maxAttempts = initStepOnFailureMaxAttempts
	}

	timeout := initStepDefaultTimeout
	if step.Timeout > 0 {
		timeout = step.Timeout
	}

	backoff := initStepOnFailureBackoff
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			state.Status = types.InitStepStatusFailed
			state.Reason = types.InitStepReasonRuntimeError
			state.Message = "context cancelled"
			c.persistInitStates(ctx, instance)
			return ctx.Err()
		}

		state.Status = types.InitStepStatusRunning
		state.Attempts = attempt
		state.StartedAt = time.Now().UTC()
		state.FinishedAt = time.Time{}
		state.ExitCode = 0
		state.Reason = ""
		state.Message = ""
		c.persistInitStates(ctx, instance)

		stepCtx, cancel := context.WithTimeout(ctx, timeout)
		exit, err := r.RunInit(stepCtx, instance, step)
		cancel()

		state.FinishedAt = time.Now().UTC()
		state.ExitCode = exit

		if err == nil && exit == 0 {
			state.Status = types.InitStepStatusSucceeded
			c.persistInitStates(ctx, instance)
			c.logger.Info("Init step succeeded",
				log.Str("instance", instance.ID),
				log.Str("step", step.Name),
				log.Int("attempt", attempt))
			return nil
		}

		// Classify failure.
		switch {
		case errors.Is(err, runner.ErrInitNotSupported):
			state.Status = types.InitStepStatusFailed
			state.Reason = types.InitStepReasonRuntimeError
			state.Message = "runner does not support init steps (RUNE-121 S5)"
			c.persistInitStates(ctx, instance)
			return err // non-retryable
		case errors.Is(err, context.DeadlineExceeded):
			state.Reason = types.InitStepReasonTimeout
			state.Message = fmt.Sprintf("attempt %d timed out after %s", attempt, timeout)
			lastErr = err
		case err != nil:
			state.Reason = types.InitStepReasonRuntimeError
			state.Message = fmt.Sprintf("attempt %d: %v", attempt, err)
			lastErr = err
		default: // exit != 0
			state.Reason = types.InitStepReasonNonZeroExit
			state.Message = fmt.Sprintf("attempt %d exited with code %d", attempt, exit)
			lastErr = fmt.Errorf("non-zero exit code %d", exit)
		}

		if step.RestartPolicy == types.InitStepRestartNever {
			state.Status = types.InitStepStatusFailed
			c.persistInitStates(ctx, instance)
			return lastErr
		}

		if attempt < maxAttempts {
			c.logger.Warn("Init step failed; will retry",
				log.Str("instance", instance.ID),
				log.Str("step", step.Name),
				log.Int("attempt", attempt),
				log.Int("max_attempts", maxAttempts),
				log.Err(lastErr))
			// Persist the interim Failed-but-retrying state so observers
			// see attempts climb.
			state.Status = types.InitStepStatusFailed
			c.persistInitStates(ctx, instance)

			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff *= 2
		}
	}

	state.Status = types.InitStepStatusFailed
	c.persistInitStates(ctx, instance)
	return lastErr
}

// persistInitStates writes the instance back to the store. Errors are
// logged but not propagated; loss of a single status update is recoverable
// and we don't want a transient store hiccup to nuke an in-flight init.
func (c *Controller) persistInitStates(ctx context.Context, instance *types.Instance) {
	if err := c.store.Update(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, instance); err != nil {
		c.logger.Warn("Failed to persist init step state",
			log.Str("instance", instance.ID),
			log.Err(err))
	}
}

// evaluateRunIf returns (run, skipReason).
//
// freshVolume  — run iff no parent volume permitted by step.Volumes
//
//	carries an InitializedFor entry for this service. The volume
//	controller clears the entry on reclaim, so `rune volume delete &&
//	rune cast` correctly re-formats while crash-recovery and `rune
//	restart` correctly skip.
//
// fileMissing  — translate runIf.path from the container mount path and stat
// it under the host source for every parent volume.
//
//	volume permitted by step.Volumes filter (or all parents if filter
//	is nil). Run iff the file is absent in every matching mount.
//	A nil/empty step.RunIf.Volume considers all permitted mounts; a
//	non-empty one restricts to that single name.
//
// always       — always run.
func (c *Controller) evaluateRunIf(
	ctx context.Context,
	step types.InitStep,
	instance *types.Instance,
	serviceKey string,
) (bool, string) {
	rt := step.RunIf.Type
	if rt == "" {
		rt = types.RunIfFreshVolume
	}
	switch rt {
	case types.RunIfAlways:
		return true, ""

	case types.RunIfFreshVolume:
		mounts := selectInitVolumes(instance, step, "")
		if len(mounts) == 0 {
			// No parent volume to anchor on — validation should have
			// rejected this; treat as not-fresh and run.
			return true, ""
		}
		for _, m := range mounts {
			if m.VolumeName == "" {
				continue // host-only mount, no Volume resource to consult
			}
			ns := m.VolumeNamespace
			if ns == "" {
				ns = instance.Namespace
			}
			var vol types.Volume
			if err := c.store.Get(ctx, types.ResourceTypeVolume, ns, m.VolumeName, &vol); err != nil {
				// Volume disappeared underneath us — not fresh, run.
				continue
			}
			if _, ok := vol.InitializedFor[serviceKey]; ok {
				return false, fmt.Sprintf("freshVolume: %s/%s already initialised for %s", ns, m.VolumeName, serviceKey)
			}
		}
		return true, ""

	case types.RunIfFileMissing:
		mounts := selectInitVolumes(instance, step, step.RunIf.Volume)
		if len(mounts) == 0 {
			// No matching mount on the instance — nothing to check, so
			// the file is trivially missing. Run.
			return true, ""
		}
		for _, m := range mounts {
			if m.Source == "" || m.MountPath == "" {
				continue
			}
			rel, err := filepath.Rel(filepath.Clean(m.MountPath), filepath.Clean(step.RunIf.Path))
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			full := filepath.Join(m.Source, m.SubPath, rel)
			if _, err := os.Stat(full); err == nil {
				return false, fmt.Sprintf("fileMissing: %s exists in volume %q", step.RunIf.Path, m.Name)
			}
		}
		return true, ""

	default:
		// Validation should reject this; treat unknown as "run" to fail
		// loudly in the runner rather than silently skip.
		return true, ""
	}
}

// selectInitVolumes returns the subset of instance volume mounts that
// match the step's `volumes` filter (nil = all, [] = none, [...] =
// named) further narrowed by an optional single-volume name (used by
// runIf.fileMissing.volume).
func selectInitVolumes(instance *types.Instance, step types.InitStep, volumeFilter string) []types.ResolvedVolumeMount {
	if instance.Metadata == nil {
		return nil
	}
	allow := func(name string) bool {
		if step.Volumes == nil {
			return true
		}
		for _, v := range step.Volumes {
			if v == name {
				return true
			}
		}
		return false
	}
	out := make([]types.ResolvedVolumeMount, 0, len(instance.Metadata.VolumeMounts))
	for _, vm := range instance.Metadata.VolumeMounts {
		if !allow(vm.Name) {
			continue
		}
		if volumeFilter != "" && vm.Name != volumeFilter {
			continue
		}
		out = append(out, vm)
	}
	return out
}
