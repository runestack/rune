// Instance-vs-service compatibility classification (drives the update
// plan). Split from instance_controller.go (RUNE-311); restructured in Phase 2 as
// pre-checks → observe → pure classify (RUNE-311 D5).

package instance

import (
	"context"
	"fmt"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
)

// CompatClass is the outcome of comparing an instance against its service's
// current spec. The distinction is the heart of RUNE-042: the old single
// boolean conflated two categories with opposite urgency, so a template
// change and a crashed container took the same code path — delete everything,
// recreate everything. A rate limit on that boolean would also have throttled
// crash recovery, which is why the split has to come first.
type CompatClass int

const (
	// CompatOK — the instance matches the spec; leave it alone.
	CompatOK CompatClass = iota

	// CompatBroken — the instance is not serving anyone (crashed, terminal
	// in the runner, unreachable, or owned by a different service). Repair
	// immediately and unbudgeted: waiting cannot help, and no traffic is
	// lost by replacing it now.
	CompatBroken

	// CompatOutdated — the instance is serving fine, it is just running an
	// older template. Replacement is voluntary, so it is what the update
	// budget governs (Phase 4). Until then, callers treat it exactly as they
	// treated `false`.
	CompatOutdated
)

// CompatVerdict carries the class plus the human-facing reason that has
// always been logged and surfaced in events.
type CompatVerdict struct {
	Class  CompatClass
	Reason string
}

// Compatible reports whether the instance needs no action — the predicate the
// pre-RUNE-042 boolean expressed.
func (v CompatVerdict) Compatible() bool { return v.Class == CompatOK }

// instanceObservation is what observeInstance learned from the runner.
// brokenReason, when non-empty, means observation itself failed (no runner,
// or the instance is unknown to it) — classified CompatBroken as-is.
type instanceObservation struct {
	status       types.InstanceStatus
	brokenReason string
}

// preClassifyInstance is the store-only prefix of classification: the checks
// that return a verdict without ever touching the runner. It runs FIRST so
// records the runner must not be asked about — notably a Stalled
// stuck-in-create record deliberately held in place by the churn guard —
// are never probed, healed, or persisted per reconcile tick (RUNE-311 D5).
// done=false means no verdict yet: observe, then classify.
func preClassifyInstance(instance *types.Instance, service *types.Service) (v CompatVerdict, done bool) {
	// Belongs to a different service: not this service's instance at all.
	if instance.ServiceID != service.ID {
		return CompatVerdict{CompatBroken, "instance belongs to different service"}, true
	}

	// Stuck-in-create record: Status=Failed (or Stalled) but a
	// container was never successfully created for this UUID
	// (precondition failure such as StorageClassMissing, missing
	// secret, image-pull error). The slot is legitimately held by
	// this record — classifying it as anything but OK would trigger
	// tombstone+recreate-with-new-UUID every reconcile tick, the
	// exact churn that RUNE-BUG-RECONCILER-CHURN-ON-STABLE-PRECONDITION-FAILURE
	// describes. Report OK so the reconciler leaves the record in
	// place; the reconciler's retry-in-place branch handles backoff
	// (Failed) or holds-without-retry (Stalled — operator must run
	// `rune restart instance` or `rune cast` to re-arm).
	//
	// This guard stays FIRST and stays OK. Everything below assumes a
	// container existed at some point.
	if instance.ContainerEverCreatedAt == nil &&
		(instance.Status == types.InstanceStatusFailed || instance.Status == types.InstanceStatusStalled) {
		return CompatVerdict{CompatOK, ""}, true
	}

	// --- liveness first: is this instance serving anyone? ---

	// Recorded terminal state.
	if instance.Status == types.InstanceStatusFailed ||
		instance.Status == types.InstanceStatusExited ||
		instance.Status == types.InstanceStatusUnknown {
		return CompatVerdict{CompatBroken,
			fmt.Sprintf("instance is in failed state: %s", string(instance.Status))}, true
	}

	return CompatVerdict{}, false
}

// observeInstance asks the runner about the instance and owns the heal
// side effects that used to hide inside classification (RUNE-311 D5): the Docker
// runner self-heals a stale ContainerID inside Status (re-resolving via
// the rune.instance.id label and mutating the in-hand instance), and a
// detected heal is persisted UNCONDITIONALLY — never gated on the later
// verdict — so the health controller's next probe dials the LIVE
// container's IP. An unpersisted heal leaves probes timing out against
// the dead container's address, the restart-churn loop PR #155 broke.
func (c *Controller) observeInstance(ctx context.Context, instance *types.Instance) instanceObservation {
	runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return instanceObservation{brokenReason: fmt.Sprintf("failed to get runner: %v", err)}
	}

	prevContainerID := instance.ContainerID
	status, err := runner.Status(ctx, instance)
	if err != nil {
		return instanceObservation{brokenReason: fmt.Sprintf("instance not found in runner: %v", err)}
	}
	if instance.ContainerID != prevContainerID {
		c.persistHealedContainerMapping(ctx, instance)
	}
	return instanceObservation{status: status}
}

// classifyObserved is the pure remainder of classification: given the
// instance, the service spec, and what the runner reported, produce the
// verdict. No store, no runner, no side effects — table-testable.
//
// Check order matters and differs from the pre-RUNE-042 function in one
// deliberate way: the runner-liveness checks run BEFORE the template
// generation check. In the old order an instance that was dead in the runner
// but also on an old template reported "template changed" and would, once the
// budget exists, be treated as a serving instance awaiting a voluntary
// replacement — counted as available, retired under budget. Classification
// has to ask "is it alive?" before "is it current?".
func classifyObserved(instance *types.Instance, service *types.Service, obs instanceObservation, logger log.Logger) CompatVerdict {
	if obs.brokenReason != "" {
		return CompatVerdict{CompatBroken, obs.brokenReason}
	}

	// Alive in the store but terminal in the runner.
	if obs.status == types.InstanceStatusExited || obs.status == types.InstanceStatusFailed {
		return CompatVerdict{CompatBroken, "instance is in terminal state in the runner"}
	}

	// --- then currency: is this instance running the current template? ---

	// Check the service TEMPLATE generation — not Generation, which also
	// advances on scale changes (RFC #129 Phase 2). Comparing Generation here
	// made every scale op recreate its surviving containers (issue #142); the
	// template counter only moves on a real template change, where
	// replacement is correct. Pre-migration state is safe by construction:
	// services from before that change have TemplateGeneration 0, which is
	// never "newer" than any recorded instance generation, so nothing bounces
	// until the next cast stamps it.
	if instance.Metadata != nil {
		if instanceGen := instance.Metadata.ServiceGeneration; instanceGen != 0 {
			if instanceGen < service.Metadata.TemplateGeneration {
				logger.Debug("Service template changed, instance is outdated",
					log.Str("instance", instance.ID),
					log.Int64("instance_generation", instanceGen),
					log.Int64("template_generation", service.Metadata.TemplateGeneration))
				return CompatVerdict{CompatOutdated,
					fmt.Sprintf("service template changed: %d -> %d", instanceGen, service.Metadata.TemplateGeneration)}
			}
		} else if service.Metadata.TemplateGeneration > 0 {
			// No recorded generation but the service's template has been
			// stamped: adopt the template by replacement.
			logger.Debug("Instance missing template generation, is outdated",
				log.Str("instance", instance.ID),
				log.Int64("template_generation", service.Metadata.TemplateGeneration))
			return CompatVerdict{CompatOutdated, "instance missing service template generation"}
		}
	}

	// For Docker-based instances, perform additional template checks. These
	// predate TemplateGeneration and are largely redundant with it now, but
	// they still catch drift on a service whose template counter has not been
	// stamped yet (pre-migration records).
	if instance.ContainerID != "" && service.Runtime == "docker" {
		if instance.Metadata != nil {
			if instance.Metadata.Image != "" {
				if instance.Metadata.Image != service.Image {
					return CompatVerdict{CompatOutdated,
						fmt.Sprintf("image changed: %s -> %s", instance.Metadata.Image, service.Image)}
				}
			} else {
				// If we can't determine the original image, be cautious and replace.
				logger.Debug("Cannot determine original image for instance, assuming outdated")
				return CompatVerdict{CompatOutdated, "cannot determine original image"}
			}
		}

		// Devices. This comparison is what actually replaces an instance
		// when resources.gpu changes: cast writes a freshly rendered spec
		// whose TemplateGeneration is zero, which disables the counter
		// check above, so the field-level fallback is the only thing left.
		//
		// Compares the device COUNT the instance holds against the count
		// the spec now asks for. A vram-only edit is invisible here — the
		// instance records which devices it holds, never how much of them
		// it was admitted for — so 20Gi to 40Gi does not replace anything.
		// Closing that needs the admitted request on the instance record.
		if service.Resources.GPU.DeviceCount() != len(instance.GPUAssignments) {
			return CompatVerdict{CompatOutdated, "gpu request changed"}
		}

		// Check for significant resource changes
		if service.Resources.CPU.Limit != "" || service.Resources.Memory.Limit != "" {
			if instance.Resources == nil ||
				(instance.Resources.CPU.Limit != service.Resources.CPU.Limit) ||
				(instance.Resources.Memory.Limit != service.Resources.Memory.Limit) {
				return CompatVerdict{CompatOutdated, "resource requirements changed"}
			}
		}

		// Check for significant environment changes. Note this is
		// one-directional — it only checks that every SERVICE env var is
		// present and equal on the instance, so a REMOVED var does not show
		// up here. TemplateGeneration does move on removal, so the case is
		// covered; this loop is the pre-migration fallback.
		if len(service.Env) > 0 {
			for key, value := range service.Env {
				// Skip RUNE internal environment variables
				if len(key) > 5 && key[:5] == "RUNE_" {
					continue
				}
				// And the device scoping: Rune writes those keys last, so a
				// spec value here is compared against something Rune wrote,
				// not against the spec. On a GPU service that never matches
				// — the instance is outdated on every pass and so is its
				// replacement, a service that replaces itself forever.
				if runner.IsGPUVisibilityVar(key) {
					continue
				}
				instanceValue, exists := instance.Environment[key]
				if !exists || instanceValue != value {
					return CompatVerdict{CompatOutdated,
						fmt.Sprintf("environment variable %s changed or missing", key)}
				}
			}
		}
	}

	// For process-based instances, perform process-specific checks
	if instance.PID != 0 && service.Runtime == "process" {
		if instance.Process != nil && service.Process != nil {
			if instance.Process.Command != service.Process.Command ||
				!areStringSlicesEqual(instance.Process.Args, service.Process.Args) {
				return CompatVerdict{CompatOutdated, "process command or arguments changed"}
			}
			if instance.Process.WorkingDir != service.Process.WorkingDir {
				return CompatVerdict{CompatOutdated, "process working directory changed"}
			}
		}
	}

	return CompatVerdict{CompatOK, ""}
}

// ClassifyInstance compares an instance against its service's current spec:
// store-only pre-checks, then runner observation (with its heal persisted
// unconditionally), then the pure classifier. See preClassifyInstance,
// observeInstance, classifyObserved.
func (c *Controller) ClassifyInstance(ctx context.Context, instance *types.Instance, service *types.Service) CompatVerdict {
	if v, done := preClassifyInstance(instance, service); done {
		return v
	}
	obs := c.observeInstance(ctx, instance)
	return classifyObserved(instance, service, obs, c.logger)
}

// IsInstanceCompatibleWithService is the boolean view of ClassifyInstance,
// kept for callers that only need "does this need action" — the reconciler's
// ensure* loops, which still delete outdated instances themselves until the
// update budget lands in Phase 4.
func (c *Controller) IsInstanceCompatibleWithService(ctx context.Context, instance *types.Instance, service *types.Service) (bool, string) {
	v := c.ClassifyInstance(ctx, instance, service)
	return v.Compatible(), v.Reason
}
