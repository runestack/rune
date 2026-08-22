// Package instance — Create-failure bookkeeping: backoff ladder, error classification,
// crash-log snapshots. Split from instance_controller.go (RUNE-311).
package instance

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// markInstanceFailedInPlace transitions a failing instance to its terminal
// tombstone state — Status=Failed, FailedAt=now, FailureReason — while
// LEAVING ITS CONTAINER ALONE. The container has already been Stopped by
// the caller; we just don't remove it, so `rune logs <id>` and
// `rune exec --debug <id>` can still address it for postmortem.
//
// In the new naming scheme each container is named
// `<namespace>-<service>-<ordinal>-<id_prefix>` where id_prefix is derived
// from the instance UUID. That suffix means a freshly-created replacement
// instance NEVER collides with this preserved container on the docker
// side, so we don't need to rename or fork a separate "tombstone"
// instance record (the rename + new-record dance of the previous design
// is gone). The Failed instance record IS the tombstone.
//
// The reconciler filters out Failed+FailedAt records when looking for a
// live instance to occupy the service's name slot, so creating the
// replacement next reconcile tick (or synchronously from RestartInstance)
// will just work.
func (c *Controller) markInstanceFailedInPlace(ctx context.Context, instance *types.Instance, restartReason RestartReason) error {
	// Capture the tail of the container's stdout/stderr before we
	// freeze the record. Without this, `rune logs <id>` falls back
	// to the LastLogs snapshot only to find the field empty —
	// defeating the whole point of preserving the postmortem.
	c.snapshotInstanceLogs(ctx, instance)
	now := time.Now()
	msg := fmt.Sprintf("Preserved for postmortem after %s", restartReason)
	// Write ONLY the tombstone status fields atomically on the fresh record, so
	// a concurrent write (e.g. the reconciler's in-place UpdateInstance) can't
	// resurrect this Failed instance by clobbering it (RFC #129 Phase 1c).
	var fresh types.Instance
	if err := c.store.UpdateFunc(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, &fresh, func() error {
		fresh.Status = types.InstanceStatusFailed
		fresh.StatusMessage = msg
		fresh.FailedAt = &now
		fresh.FailureReason = string(restartReason)
		fresh.UpdatedAt = now
		return nil
	}, store.WithHealthController()); err != nil {
		return fmt.Errorf("mark instance failed: %w", err)
	}
	instance.Status = types.InstanceStatusFailed
	instance.StatusMessage = msg
	instance.FailedAt = &now
	instance.FailureReason = string(restartReason)
	instance.UpdatedAt = now
	c.logger.Info("Marked instance Failed (tombstoned in-place)",
		log.Str("instance", instance.ID),
		log.Str("container_id", instance.ContainerID),
		log.Str("reason", string(restartReason)))
	return nil
}

// recordCreateFailure persists the failure of a CreateInstance attempt
// onto the existing instance record (same UUID, same Name) so operators
// can see why an instance is stuck via `rune get instance -o yaml`.
// Without this, the failure reason only lives in transient runed logs
// and the record stays at Status=Pending with no detail.
//
// Sets Status=Failed, FailedAt=now, FailureReason, StatusMessage, and
// increments CreateAttempts. Whether the reconciler retries this same
// record in place or tombstones and recreates is decided downstream
// via the ContainerEverCreatedAt gate: nil means create never
// succeeded (precondition failure — operator must fix), non-nil means
// the container existed at some point (transient — recreate).
func (c *Controller) recordCreateFailure(ctx context.Context, instance *types.Instance, err error, reason string) {
	if instance == nil {
		return
	}
	now := time.Now()

	// Apply the failure fields atomically on the fresh record. CreateAttempts is
	// a counter, so it MUST increment the current stored value (not a possibly
	// stale snapshot) — UpdateFunc re-reads and re-applies on conflict. Only
	// this call's own fields are touched, so a concurrent write isn't clobbered
	// (RFC #129 Phase 1c). Deliberately do NOT set FailedAt: that marks a
	// tombstone (a container that ran and was preserved), which the retention GC
	// keys off; a stuck-in-create record is neither a tombstone nor evictable.
	stalled := false
	var fresh types.Instance
	if updateErr := c.store.UpdateFunc(ctx, types.ResourceTypeInstance, instance.Namespace, instance.ID, &fresh, func() error {
		fresh.CreateAttempts++
		fresh.FailureReason = reason
		// Flip to Stalled when retries are exhausted so operators get a clear
		// "stop waiting, take action" signal; while Stalled, NextCreateAttemptAt
		// stays nil — the reconciler never auto-retries.
		if fresh.CreateAttempts >= maxCreateAttempts {
			applyInstanceStatus(&fresh, types.InstanceStatusStalled, reason, err.Error())
			fresh.NextCreateAttemptAt = nil
			stalled = true
		} else {
			applyInstanceStatus(&fresh, types.InstanceStatusFailed, reason, err.Error())
			next := now.Add(createBackoffFor(fresh.CreateAttempts))
			fresh.NextCreateAttemptAt = &next
			stalled = false
		}
		return nil
	}, store.WithReconciler()); updateErr != nil {
		c.logger.Error("Failed to persist create-failure status on instance",
			log.Str("instance", instance.ID),
			log.Str("reason", reason),
			log.Err(updateErr))
		return
	}

	// Reflect the persisted state on the caller's copy, then emit/log once
	// (side effects must not repeat across UpdateFunc retries).
	*instance = fresh
	if stalled {
		c.logger.Warn("Instance create retries exhausted; marking Stalled",
			log.Str("instance", instance.ID),
			log.Str("reason", reason),
			log.Int("attempts", instance.CreateAttempts))
	}
	c.emit(types.EventLevelError, instance, reason, err.Error())
}

// Backoff schedule for CreateInstance retries on a stuck-in-create
// record. Reconciler tick is ~30s (see reconciler.go), so the
// schedule 30s → 1m → 2m → 4m → 5m (cap) gives ~17 min total before
// the 6th attempt flips the record to Stalled. The intent: precondition
// errors that require operator action (StorageClassMissing, missing
// secret, image-pull failures) take minutes to resolve and shouldn't
// hammer the volume controller / image registry every tick.
const (
	maxCreateAttempts = 6
	createBaseBackoff = 30 * time.Second
	createMaxBackoff  = 5 * time.Minute
)

// snapshotLogBytes caps the per-instance stdout/stderr snapshot
// captured into Instance.LastLogs before the runner removes the
// container. 200KB matches the runefile config comment and is
// enough to carry the tail of a typical crash trace (a couple
// hundred lines of stack), without bloating the store.
const snapshotLogBytes = 200 * 1024

// snapshotInstanceLogs reads the tail of an instance's runner logs
// and stamps Instance.LastLogs / LastLogsCapturedAt / LastLogsTruncated
// so `rune logs <id>` and the service-level tombstone fallback can
// still serve them after the container is gone. Best-effort: if the
// runner can't be reached or has no logs (the common cases where the
// snapshot would have been empty anyway), this is a no-op.
//
// Designed to be called from DeleteInstance and markInstanceFailedInPlace
// — i.e. exactly the lifecycle moments where we are ABOUT to lose
// the container and therefore the live log stream.
func (c *Controller) snapshotInstanceLogs(ctx context.Context, instance *types.Instance) {
	if instance == nil {
		return
	}
	// Skip when there has never been a container — nothing to snapshot.
	// Accept either ContainerEverCreatedAt (set by PR2) OR a non-empty
	// ContainerID (covers legacy records created before PR2 where the
	// new field is nil but a container existed). Without the
	// ContainerID fallback, services that predate dev.75 never get
	// LastLogs captured, so the GetServiceLogs fallback has nothing
	// to serve.
	if instance.ContainerEverCreatedAt == nil && instance.ContainerID == "" {
		return
	}
	// Already snapshotted? Don't overwrite — keep the original
	// crash output rather than replacing it with whatever the
	// reconciler picks up later.
	if len(instance.LastLogs) > 0 {
		return
	}
	_runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return
	}
	rc, err := _runner.GetLogs(ctx, instance, runner.LogOptions{Tail: 0})
	if err != nil || rc == nil {
		return
	}
	defer rc.Close()
	// Bounded read: at most snapshotLogBytes+1 so we can detect
	// truncation cheaply.
	limited := io.LimitReader(rc, int64(snapshotLogBytes)+1)
	buf, err := io.ReadAll(limited)
	if err != nil || len(buf) == 0 {
		return
	}
	truncated := false
	if len(buf) > snapshotLogBytes {
		buf = buf[:snapshotLogBytes]
		truncated = true
	}
	now := time.Now()
	instance.LastLogs = buf
	instance.LastLogsCapturedAt = &now
	instance.LastLogsTruncated = truncated
	c.logger.Debug("Captured LastLogs snapshot",
		log.Str("instance", instance.ID),
		log.Int("bytes", len(buf)),
		log.Bool("truncated", truncated))
}

// createBackoffFor returns the delay to wait before the (attempt+1)-th
// retry. Exponential with a cap at createMaxBackoff; attempt is
// 1-based. Mirrors volumeController.backoffFor.
func createBackoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := createBaseBackoff
	for i := 1; i < attempt && d < createMaxBackoff; i++ {
		d *= 2
	}
	if d > createMaxBackoff {
		d = createMaxBackoff
	}
	return d
}

// classifyCreateError maps an error returned during CreateInstance to
// a short, machine-friendly FailureReason slug surfaced on the
// instance record. New reasons can be added without breaking
// consumers — unrecognised errors fall through to "CreateFailed".
func classifyCreateError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "StorageClassMissing"):
		return "StorageClassMissing"
	case strings.Contains(msg, "InvalidParameters"):
		return "VolumeInvalidParameters"
	case strings.Contains(msg, "ProvisionRetriesExhausted"):
		return "VolumeProvisionStalled"
	case strings.Contains(msg, "volume ") && strings.Contains(msg, "is not ready"):
		return "VolumeNotReady"
	// Volume-mount resolution failures travel inside the generic
	// "failed to resolve secret and config mounts:" wrapper, so they
	// must be classified before the "resolve secret" case below —
	// otherwise a volume-not-mounted error is mislabelled SecretNotFound.
	case strings.Contains(msg, "resolve volume mount"),
		strings.Contains(msg, "not yet mounted"):
		return "VolumeNotReady"
	case strings.Contains(msg, "resolve secret"):
		return "SecretNotFound"
	case strings.Contains(msg, "resolve config"):
		return "ConfigmapNotFound"
	case strings.Contains(msg, "prepare environment variables"):
		return "EnvResolveFailed"
	case strings.Contains(msg, "init steps failed"):
		return "InitStepFailed"
	case strings.Contains(msg, "get runner"):
		return "RunnerUnavailable"
	case strings.Contains(msg, "failed to create instance:"):
		return "RunnerCreateError"
	case strings.Contains(msg, "failed to start instance"):
		return "RunnerStartError"
	}
	return "CreateFailed"
}
