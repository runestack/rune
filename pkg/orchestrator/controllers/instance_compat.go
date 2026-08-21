// Package controllers — Instance-vs-service compatibility classification (drives the update
// plan). Split from instance_controller.go (RUNE-311).
package controllers

import (
	"context"
	"fmt"

	"github.com/runestack/rune/pkg/log"
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

// classifyInstance compares an instance against its service's current spec.
//
// Check order matters and differs from the pre-RUNE-042 function in one
// deliberate way: the runner-liveness checks now run BEFORE the template
// generation check. In the old order an instance that was dead in the runner
// but also on an old template reported "template changed" and would, once the
// budget exists, be treated as a serving instance awaiting a voluntary
// replacement — counted as available, retired under budget. Classification
// has to ask "is it alive?" before "is it current?".
func (c *instanceController) classifyInstance(ctx context.Context, instance *types.Instance, service *types.Service) CompatVerdict {
	// Belongs to a different service: not this service's instance at all.
	if instance.ServiceID != service.ID {
		return CompatVerdict{CompatBroken, "instance belongs to different service"}
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
		return CompatVerdict{CompatOK, ""}
	}

	// --- liveness first: is this instance serving anyone? ---

	// Recorded terminal state.
	if instance.Status == types.InstanceStatusFailed ||
		instance.Status == types.InstanceStatusExited ||
		instance.Status == types.InstanceStatusUnknown {
		return CompatVerdict{CompatBroken,
			fmt.Sprintf("instance is in failed state: %s", string(instance.Status))}
	}

	// Get the current runner for the instance.
	runner, err := c.runnerManager.GetInstanceRunner(instance)
	if err != nil {
		return CompatVerdict{CompatBroken, fmt.Sprintf("failed to get runner: %v", err)}
	}

	// Check if the instance still exists in the runner. The Docker
	// runner self-heals a stale ContainerID here (re-resolving via the
	// rune.instance.id label and mutating the in-hand instance); if it
	// did, persist the healed mapping so the health controller's next
	// probe dials the LIVE container's IP — an unpersisted heal leaves
	// probes timing out against the dead container's address, which is
	// the restart-churn loop this exists to break.
	prevContainerID := instance.ContainerID
	status, err := runner.Status(ctx, instance)
	if err != nil {
		return CompatVerdict{CompatBroken, fmt.Sprintf("instance not found in runner: %v", err)}
	}
	if instance.ContainerID != prevContainerID {
		c.persistHealedContainerMapping(ctx, instance)
	}

	// Alive in the store but terminal in the runner.
	if status == types.InstanceStatusExited || status == types.InstanceStatusFailed {
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
				c.logger.Debug("Service template changed, instance is outdated",
					log.Str("instance", instance.ID),
					log.Int64("instance_generation", instanceGen),
					log.Int64("template_generation", service.Metadata.TemplateGeneration))
				return CompatVerdict{CompatOutdated,
					fmt.Sprintf("service template changed: %d -> %d", instanceGen, service.Metadata.TemplateGeneration)}
			}
		} else if service.Metadata.TemplateGeneration > 0 {
			// No recorded generation but the service's template has been
			// stamped: adopt the template by replacement.
			c.logger.Debug("Instance missing template generation, is outdated",
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
				c.logger.Debug("Cannot determine original image for instance, assuming outdated")
				return CompatVerdict{CompatOutdated, "cannot determine original image"}
			}
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

// isInstanceCompatibleWithService is the boolean view of classifyInstance,
// kept for callers that only need "does this need action" — the reconciler's
// ensure* loops, which still delete outdated instances themselves until the
// update budget lands in Phase 4.
func (c *instanceController) isInstanceCompatibleWithService(ctx context.Context, instance *types.Instance, service *types.Service) (bool, string) {
	v := c.classifyInstance(ctx, instance, service)
	return v.Compatible(), v.Reason
}
