// bringing instance counts to desired: stateful ordinals,
// stateless slots, and update planning.

package reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/log"
	instancectl "github.com/runestack/rune/pkg/orchestrator/instance"
	"github.com/runestack/rune/pkg/types"
)

// ensureServiceInstances makes sure we have the right number of instances and they're up to date
func (r *Reconciler) ensureServiceInstances(ctx context.Context, service *types.Service) error {
	// If the service declares dependencies, gate instance CREATION until they
	// are ready — creation only, not the whole pass.
	//
	// This used to return early, which was harmless while scale-down ran
	// ahead of it in reconcileService. Now that stateless excess removal
	// lives in the plan, an early return also suppresses retirement: a
	// service whose dependency went unready would ignore `rune scale`
	// entirely, keeping every instance up while the operator watched the
	// command time out. Blocking creates while still honouring the desired
	// scale downward is both what the log line claims and the safer half.
	createsBlocked := false
	if len(service.Dependencies) > 0 {
		ready, err := r.dependenciesReady(ctx, service)
		if err != nil {
			r.logger.Error("Dependency readiness check failed",
				log.Str("service", service.Name),
				log.Err(err))
			// Be safe: do not create against an unknown dependency state.
			createsBlocked = true
		} else if !ready {
			r.logger.Info("Delaying instance creation; dependencies not ready",
				log.Str("service", service.Name),
				log.Str("namespace", service.Namespace))
			createsBlocked = true
		}
	}

	// Get existing instances for this service
	instanceData, err := r.getServiceInstances(ctx, service)
	if err != nil {
		return err
	}
	r.logger.Debug("Ensuring service instances",
		log.Str("service", service.Name),
		log.Int("desired", service.Scale),
		log.Int("current", len(instanceData.Instances)))

	// Stateful services (per-replica volume claimTemplates) keep stable
	// {service}-{ordinal} slot names so replicas rebind their volumes across
	// restarts; stateless services get unique {service}-{shorthash} names per
	// lifetime (#84). The two need different reconcile identity models — slot
	// matching vs. count-based — so dispatch here.
	if serviceHasStableIdentity(service) {
		// The stateful path only ever creates or replaces in-slot, so the
		// old all-or-nothing gate is still the right shape for it.
		if createsBlocked {
			return nil
		}
		return r.ensureStatefulInstances(ctx, service, instanceData)
	}
	return r.ensureStatelessInstances(ctx, service, instanceData, createsBlocked)
}

// ensureStatefulInstances reconciles a service whose replicas have stable
// ordinal identity. For each slot 0..Scale-1 it looks up the {service}-{ordinal}
// instance by name: a compatible one is reconciled in place, an incompatible one
// is deleted and recreated in the same slot, and a missing one is created. This
// is the StatefulSet-style model where the name *is* the slot key.
func (r *Reconciler) ensureStatefulInstances(ctx context.Context, service *types.Service, instanceData *ServiceInstanceData) error {
	for i := 0; i < service.Scale; i++ {
		// Generate instance name
		instanceName := generateInstanceName(service, i)

		// Check if this instance already exists and is compatible
		var existingInstance *types.Instance
		for j := range instanceData.Instances {
			if instanceData.Instances[j].Name == instanceName {
				// Use the existing compatibility check function
				isCompatible, reason := r.instanceController.IsInstanceCompatibleWithService(ctx, &instanceData.Instances[j], service)
				if isCompatible {
					existingInstance = &instanceData.Instances[j]
					break
				} else {
					r.logger.Info("Instance incompatible, will recreate",
						log.Str("service", service.Name),
						log.Str("instance", instanceName),
						log.Str("reason", reason))
					// Always remove the old instance from health monitoring
					// (regardless of current service.Health configuration)
					r.healthController.RemoveInstance(instanceData.Instances[j].ID)
					// Delete the old instance
					if err := r.instanceController.DeleteInstance(ctx, &instanceData.Instances[j]); err != nil {
						r.logger.Error("Failed to delete old instance during recreation",
							log.Str("instance", instanceData.Instances[j].ID),
							log.Err(err))
					}
				}
			}
		}

		if existingInstance != nil {
			// Update existing instance
			if err := r.reconcileExistingInstance(ctx, service, existingInstance); err != nil {
				r.logger.Error("Failed to reconcile existing instance",
					log.Str("service", service.Name),
					log.Str("instance", instanceName),
					log.Err(err))
				// Continue with other instances
			}
			continue
		}

		r.logger.Info("creating new instance", log.Json("instanceName", instanceName))
		// Create a new instance — i is the per-replica slot ordinal.
		if err := r.createNewInstance(ctx, service, instanceName, i); err != nil {
			r.logger.Error("Failed to create new instance",
				log.Str("service", service.Name),
				log.Str("instance", instanceName),
				log.Err(err))
			// Continue with other instances
		}
	}

	return nil
}

// ensureStatelessInstances reconciles a service whose replicas have no stable
// identity, through the update planner (RUNE-042 Phase 4).
//
// Before this, the loop deleted EVERY incompatible instance and then created
// replacements — which is why a template change took the whole service down
// at once. Now each instance is classified, the planner decides what may
// happen this tick within the availability budget, and this function only
// executes that decision.
//
// Scale-down is part of the same decision (the planner returns excess
// retirements), so there is no separate scale-down pass to fight the surge.
func (r *Reconciler) ensureStatelessInstances(ctx context.Context, service *types.Service, instanceData *ServiceInstanceData, createsBlocked bool) error {
	// Finish any teardown that was abandoned mid-flight before planning, or
	// the stranded record occupies a slot no plan can free.
	r.reapStuckTerminating(ctx, service, instanceData)

	plan, views := r.planServiceUpdate(ctx, service, instanceData)

	// 1. Retire, oldest/least-valuable first as the planner ordered them.
	//
	// Withdraw the whole set from the dataplane in one publish first, and
	// take ONE shared drain window for all of them (RUNE-042 §4). Retiring
	// serially would pay a full drain per instance — 8 × (5s drain + up to
	// 10s stop) ≈ two minutes of one of only four reconcile workers for a
	// `recreate` deploy or a wide scale-down, during which this service
	// creates nothing and other services wait. The per-instance
	// DeleteInstance calls below then see Terminating and skip their own
	// drains.
	if len(plan.Retire) > 1 {
		r.instanceController.WithdrawServiceInstances(ctx, service, plan.Retire)
	}
	for _, inst := range plan.Retire {
		r.logger.Info("Retiring instance",
			log.Str("service", service.Name),
			log.Str("instance", inst.Name),
			log.Str("reason", plan.Reason))
		r.emitService(ctx, service, types.EventLevelInfo, eventInstanceRetired,
			fmt.Sprintf("retired %s: %s", inst.Name, plan.Reason))
		r.healthController.RemoveInstance(inst.ID)
		if err := r.instanceController.DeleteInstance(ctx, inst); err != nil {
			r.logger.Error("Failed to retire instance",
				log.Str("instance", inst.ID), log.Err(err))
		}
	}

	// 2. Repair broken instances — unbudgeted, because they serve nobody.
	retired := make(map[string]bool, len(plan.Retire))
	for _, inst := range plan.Retire {
		retired[inst.ID] = true
	}
	for _, inst := range plan.Repair {
		r.logger.Info("Replacing broken instance",
			log.Str("service", service.Name),
			log.Str("instance", inst.Name))
		r.healthController.RemoveInstance(inst.ID)
		if err := r.instanceController.DeleteInstance(ctx, inst); err != nil {
			r.logger.Error("Failed to remove broken instance",
				log.Str("instance", inst.ID), log.Err(err))
		}
	}

	// 3. Reconcile the survivors in place (health monitoring, env drift,
	//    generation stamp). UpdateInstance leaves outdated instances alone —
	//    their replacement is the planner's call, made above.
	taken := make(map[string]bool, len(views))
	for i := range views {
		inst := views[i].Instance
		if retired[inst.ID] || views[i].Class == instancectl.CompatBroken {
			continue
		}
		if err := r.reconcileExistingInstance(ctx, service, inst); err != nil {
			r.logger.Error("Failed to reconcile existing instance",
				log.Str("service", service.Name),
				log.Str("instance", inst.Name),
				log.Err(err))
			// Continue with other instances
		}
		taken[inst.Name] = true
	}

	// 4. Create what the plan allows: replacements for retired/broken
	//    instances plus any shortfall against the desired scale. Ordinal is
	//    not meaningful for a stateless service (no per-replica volume
	//    binding); pass the running index so the field is populated
	//    deterministically.
	total := len(plan.Repair) + plan.Create
	if createsBlocked && total > 0 {
		r.logger.Info("Holding instance creation; dependencies not ready",
			log.Str("service", service.Name),
			log.Int("would_create", total))
		total = 0
	}
	for i := 0; i < total; i++ {
		instanceName := generateHashInstanceName(service, taken)
		taken[instanceName] = true
		r.logger.Info("creating new instance", log.Json("instanceName", instanceName))
		if err := r.createNewInstance(ctx, service, instanceName, len(taken)-1); err != nil {
			r.logger.Error("Failed to create new instance",
				log.Str("service", service.Name),
				log.Str("instance", instanceName),
				log.Err(err))
			// Continue with other instances
		}
	}

	return nil
}

// planServiceUpdate classifies the service's instances and asks the planner
// what may happen this tick. Returns the plan and the classified views, which
// the caller reuses so nothing is classified twice (each classification hits
// the runner).
func (r *Reconciler) planServiceUpdate(ctx context.Context, service *types.Service, instanceData *ServiceInstanceData) (updatePlan, []instanceView) {
	params := service.ResolveUpdateParams()
	now := time.Now()

	views := make([]instanceView, 0, len(instanceData.Instances))
	for i := range instanceData.Instances {
		inst := &instanceData.Instances[i]
		verdict := r.instanceController.ClassifyInstance(ctx, inst, service)
		v := newInstanceView(inst, verdict.Class, params.MinReady, now)
		if verdict.Class != instancectl.CompatOK {
			r.logger.Debug("Instance classified",
				log.Str("service", service.Name),
				log.Str("instance", inst.Name),
				log.Str("class", classNames[verdict.Class]),
				log.Str("reason", verdict.Reason))
		}
		views = append(views, v)
	}

	return planUpdate(updateInput{
		Scale:     service.Scale,
		Params:    params,
		Instances: views,
		Now:       now,
	}), views
}

// classNames renders a instancectl.CompatClass for logs.
var classNames = map[instancectl.CompatClass]string{
	instancectl.CompatOK:       "ok",
	instancectl.CompatBroken:   "broken",
	instancectl.CompatOutdated: "outdated",
}
