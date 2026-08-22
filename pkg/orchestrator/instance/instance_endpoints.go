// Dataplane endpoint publishing and drain: republish, signature dedup,
// batch withdraw. Split from instance_controller.go (RUNE-311).

package instance

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// drainAfterWithdraw blocks for the owning service's drain window while
// just-withdrawn instances finish their
// in-flight work. Call it AFTER the instances have been persisted in a
// non-Running state and the endpoint set republished — the pause is what
// gives the async withdrawal time to propagate and existing requests time
// to complete before the container receives SIGTERM.
//
// No-ops when nothing could have been routed there: no dataplane publisher
// (dev/standalone), a service with no ports, or wasServing false (the
// instance was not Running when its teardown began — only Running
// instances are ever published, see republishService).
func (c *Controller) drainAfterWithdraw(ctx context.Context, service *types.Service, wasServing bool, target string) {
	publisher, _ := c.endpointBinding()
	if !wasServing || publisher == nil || service == nil || len(service.Ports) == 0 {
		return
	}
	// Per-service grace (types.Service.DrainWindow): defaults to
	// DefaultDrainSeconds and is floored at MinDrainSeconds, so a spec of 0
	// still leaves time for the withdrawal to propagate.
	window := service.DrainWindow()
	if c.drainWindowOverride > 0 {
		window = c.drainWindowOverride
	}
	c.logger.Info("Draining withdrawn instance(s) before stop",
		log.Str("target", target),
		log.Duration("window", window))
	select {
	case <-time.After(window):
	case <-ctx.Done():
	}
}

// instanceEndpointIP returns the routable IP for endpoint publishing.
// It prefers persisted metadata, then asks the runner (Docker inspect),
// and in both cases ensures the discovered IP is persisted to the
// instance's top-level IP field as well as Metadata.ContainerIP.
func (c *Controller) instanceEndpointIP(ctx context.Context, inst *types.Instance) string {
	if inst == nil {
		return ""
	}

	ip := ""
	if inst.Metadata != nil && inst.Metadata.ContainerIP != "" {
		ip = inst.Metadata.ContainerIP
	} else if r, err := c.runnerManager.GetInstanceRunner(inst); err == nil {
		if p, ok := r.(runner.IPProvider); ok {
			if got, ipErr := p.InstanceIP(ctx, inst); ipErr == nil {
				ip = got
			}
		}
	}
	if ip == "" {
		return ""
	}

	c.persistInstanceIP(ctx, inst, ip)
	return ip
}

// persistInstanceIP records ip on both Instance.IP (the operator-facing
// field surfaced by `rune get`/`describe`) and Metadata.ContainerIP (used
// for VIP routing / probes). Historically only ContainerIP was written,
// leaving Instance.IP permanently empty in CLI output. The write targets
// a freshly-loaded copy — the in-hand `inst` was listed earlier in the
// republish pass and writing it back whole would clobber a concurrent
// status update (the store has no CAS); same load-modify-write discipline
// promoteToRunningOnReady uses. The in-hand copy is kept in sync so the
// rest of this pass sees the IP even if the store write is skipped/fails.
func (c *Controller) persistInstanceIP(ctx context.Context, inst *types.Instance, ip string) {
	var fresh types.Instance
	if err := c.store.Get(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &fresh); err == nil {
		if fresh.Metadata == nil {
			fresh.Metadata = &types.InstanceMetadata{}
		}
		if fresh.Metadata.ContainerIP != ip || fresh.IP != ip {
			fresh.Metadata.ContainerIP = ip
			fresh.IP = ip
			if err := c.store.Update(ctx, types.ResourceTypeInstance, fresh.Namespace, fresh.ID, &fresh); err != nil {
				c.logger.Debug("persistInstanceIP: persist failed",
					log.Str("instance", inst.Name),
					log.Err(err))
			}
		}
	}
	if inst.Metadata == nil {
		inst.Metadata = &types.InstanceMetadata{}
	}
	inst.Metadata.ContainerIP = ip
	inst.IP = ip
}

// persistHealedContainerMapping writes a runner-healed instance→container
// mapping (ContainerID + ContainerIP, see DockerRunner.Status) back to the
// store. Without this the heal only lives on the reconcile pass's in-hand
// copy: the health controller re-reads the record before every probe and
// would keep dialing the dead container's IP — probe timeout → restart →
// churn. CAS via UpdateFunc so a concurrent status write is never clobbered;
// best-effort because the next compat check heals again if the write loses.
func (c *Controller) persistHealedContainerMapping(ctx context.Context, inst *types.Instance) {
	var fresh types.Instance
	if err := c.store.UpdateFunc(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &fresh, func() error {
		if fresh.ContainerID == inst.ContainerID {
			return store.ErrSkipUpdate // another pass already persisted the heal
		}
		fresh.ContainerID = inst.ContainerID
		if fresh.Metadata == nil {
			fresh.Metadata = &types.InstanceMetadata{}
		}
		if inst.Metadata != nil && inst.Metadata.ContainerIP != "" {
			fresh.Metadata.ContainerIP = inst.Metadata.ContainerIP
			fresh.IP = inst.Metadata.ContainerIP
		}
		fresh.UpdatedAt = time.Now()
		return nil
	}, store.WithOrchestrator()); err != nil {
		c.logger.Warn("Failed to persist healed container mapping",
			log.Str("instance", inst.ID),
			log.Err(err))
		return
	}
	c.logger.Info("Persisted healed container mapping",
		log.Str("instance", inst.ID),
		log.Str("container_id", inst.ContainerID))
}

// republishService recomputes the endpoint set for a service from
// the current store contents and publishes it. Best-effort: errors
// are logged but never surfaced because a failure to publish must
// not roll back the runner-side lifecycle transition that already
// succeeded.
func (c *Controller) republishService(ctx context.Context, service *types.Service) {
	publisher, nodeID := c.endpointBinding()
	if publisher == nil || service == nil {
		return
	}
	var instances []*types.Instance
	if err := c.store.List(ctx, types.ResourceTypeInstance, service.Namespace, &instances); err != nil {
		c.logger.Warn("republishService: list instances failed",
			log.Str("service", service.Name),
			log.Err(err))
		return
	}
	// The endpoint set carries only the service's primary (first)
	// port. That is sufficient because each dataplane VIP listener
	// derives its own target port from the service spec (see
	// openListener) rather than from the endpoint — the endpoint just
	// needs to advertise the container IP. Multi-port services are
	// therefore routed correctly without per-port endpoint entries.
	primaryPort := 0
	primaryProto := "TCP"
	if len(service.Ports) > 0 {
		primaryPort = service.Ports[0].Port
		if service.Ports[0].TargetPort != 0 {
			primaryPort = service.Ports[0].TargetPort
		}
		if service.Ports[0].Protocol != "" {
			primaryProto = service.Ports[0].Protocol
		}
	}
	eps := make([]types.Endpoint, 0)
	for _, inst := range instances {
		if inst == nil || inst.ServiceName != service.Name {
			continue
		}
		if inst.Status != types.InstanceStatusRunning {
			continue
		}
		ip := c.instanceEndpointIP(ctx, inst)
		if ip == "" {
			continue
		}
		md := map[string]string{}
		if nodeID != "" {
			md["node_id"] = nodeID
		}
		eps = append(eps, types.Endpoint{
			InstanceID: inst.ID,
			IP:         ip,
			Port:       primaryPort,
			Protocol:   primaryProto,
			Metadata:   md,
			Healthy:    true,
		})
	}
	// Skip the publish when the endpoint set is byte-identical to the
	// last one we published for this service. reconcileService calls
	// republishService every tick; without this a steady-state cluster
	// would still append a no-op mutation to the OrderedLog per service
	// per tick. The signature is recorded only after a successful
	// publish so a failed publish is retried on the next tick.
	sig := endpointsSignature(eps)
	c.publishedMu.Lock()
	prev, seen := c.lastPublished[service.ID]
	c.publishedMu.Unlock()
	if seen && prev == sig {
		return
	}
	if err := publisher.PublishService(ctx, service, eps); err != nil {
		c.logger.Warn("republishService: publish failed",
			log.Str("service", service.Name),
			log.Err(err))
		return
	}
	c.publishedMu.Lock()
	c.lastPublished[service.ID] = sig
	c.publishedMu.Unlock()
}

// endpointsSignature returns a deterministic, order-independent string
// identifying an endpoint set, used by republishService to dedup
// no-op publishes. Endpoints are sorted because republishService
// builds them from a store List whose order is not guaranteed stable.
func endpointsSignature(eps []types.Endpoint) string {
	parts := make([]string, 0, len(eps))
	for _, ep := range eps {
		parts = append(parts, fmt.Sprintf("%s|%s|%d|%s|%t",
			ep.InstanceID, ep.IP, ep.Port, ep.Protocol, ep.Healthy))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// RepublishServiceByInstance is the exported entry point delegating to
// the private republishServiceByInstance — see that function for the
// full semantics. Used by external callers (e.g. the health controller)
// that need to refresh the data-plane endpoint set after an instance
// reachability change.
func (c *Controller) RepublishServiceByInstance(ctx context.Context, instance *types.Instance) {
	c.republishServiceByInstance(ctx, instance)
}

// RepublishService implements Controller.
func (c *Controller) RepublishService(ctx context.Context, service *types.Service) {
	c.republishService(ctx, service)
}

// republishServiceByInstance is a convenience wrapper that loads the
// owning service from the store before delegating to republishService.
// Used by lifecycle methods (Stop/Delete) that hold an Instance but
// not its Service.
func (c *Controller) republishServiceByInstance(ctx context.Context, instance *types.Instance) {
	if publisher, _ := c.endpointBinding(); publisher == nil || instance == nil || instance.ServiceName == "" {
		return
	}
	var svc types.Service
	if err := c.store.Get(ctx, types.ResourceTypeService, instance.Namespace, instance.ServiceName, &svc); err != nil {
		c.logger.Debug("republishServiceByInstance: service lookup failed",
			log.Str("service", instance.ServiceName),
			log.Err(err))
		return
	}
	c.republishService(ctx, &svc)
}

// republishLocalInstances rebuilds the per-node InstanceIdentity
// table from current store state across all namespaces and pushes
// it. Best-effort.
func (c *Controller) republishLocalInstances(ctx context.Context) {
	publisher, nodeID := c.endpointBinding()
	if publisher == nil || nodeID == "" {
		return
	}
	running, err := c.CollectRunningInstances(ctx)
	if err != nil {
		c.logger.Warn("republishLocalInstances: collectRunning failed", log.Err(err))
		return
	}
	table := make(map[string]types.InstanceIdentity, len(running))
	for id, ri := range running {
		if ri == nil || ri.Instance == nil {
			continue
		}
		ip := c.instanceEndpointIP(ctx, ri.Instance)
		if ip == "" {
			continue
		}
		table[ip] = types.InstanceIdentity{
			InstanceID: id,
			Service:    ri.Instance.ServiceName,
			Namespace:  ri.Instance.Namespace,
		}
	}
	if err := publisher.PublishLocalInstances(ctx, nodeID, table); err != nil {
		c.logger.Warn("republishLocalInstances: publish failed", log.Err(err))
	}
}

// WithdrawServiceInstances removes a set of instances from the dataplane
// endpoint set in one publish and takes ONE shared drain window for all of
// them (RUNE-042 §4: whole-service teardowns drain in batch, not in series —
// a per-instance drain would add len(instances) × drainWindow to a teardown
// whose instances are all being withdrawn anyway). Each Running instance is
// flipped to Terminating first, so the per-instance teardown that follows
// (StopInstance/DeleteInstance) sees a non-Running status and skips its own
// drain. Best-effort; never fails the teardown.
func (c *Controller) WithdrawServiceInstances(ctx context.Context, service *types.Service, instances []*types.Instance) {
	anyServing := false
	for _, inst := range instances {
		if inst == nil || inst.Status != types.InstanceStatusRunning {
			continue
		}
		var fresh types.Instance
		if err := c.store.UpdateFunc(ctx, types.ResourceTypeInstance, inst.Namespace, inst.ID, &fresh, func() error {
			if fresh.Status != types.InstanceStatusRunning {
				return store.ErrSkipUpdate
			}
			fresh.Status = types.InstanceStatusTerminating
			fresh.StatusMessage = "Withdrawn from routing; draining"
			fresh.UpdatedAt = time.Now()
			return nil
		}); err != nil {
			c.logger.Warn("Failed to mark instance Terminating during batch withdrawal",
				log.Str("instance", inst.ID),
				log.Err(err))
			continue
		}
		anyServing = true
		inst.Status = types.InstanceStatusTerminating
	}
	if !anyServing {
		return
	}
	if service != nil {
		c.republishService(ctx, service)
	}
	c.republishLocalInstances(ctx)
	target := "batch"
	if service != nil {
		target = service.Namespace + "/" + service.Name + " (batch)"
	}
	c.drainAfterWithdraw(ctx, service, true, target)
}
