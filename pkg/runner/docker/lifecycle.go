// Container lifecycle: Create/Start/Stop/Remove/Status/List, the
// label-based container-ID heal, and name-conflict retry. Split from
// runner.go (RUNE-312).

package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/runestack/rune/pkg/log"
	runetypes "github.com/runestack/rune/pkg/types"
)

// Create creates a new container but does not start it. The container name
// is `<namespace>-<instance.Name>-<id_prefix>` where id_prefix starts at 8
// hex chars of the instance UUID and extends only when docker reports a
// name collision — git-style abbreviation. The UUID suffix:
//
//   - Eliminates cross-namespace collisions (the `landing-0` bug):
//     prod-landing-0-<id> and dev-landing-0-<id> never collide.
//   - Eliminates the tombstone-rename dance: a Failed instance keeps its
//     container forever, and the freshly-created replacement gets its
//     own UUID-suffixed name without colliding.
//   - Stays human-readable: `docker ps` shows
//     "prod-landing-0-5d68e677" which encodes ns/service/ordinal/id.
//
// Container lookups in this runner go through `getContainerID` which
// prefers `instance.ContainerID` over labels, so dynamic-length names do
// not complicate any code path.
func (r *DockerRunner) Create(ctx context.Context, instance *runetypes.Instance) error {
	if instance == nil {
		return fmt.Errorf("invalid instance: nil pointer")
	}

	// Create container config and host config
	containerConfig, hostConfig, err := r.instanceToContainerConfig(instance)
	if err != nil {
		return fmt.Errorf("failed to create container configuration: %w", err)
	}

	// Pull the image first
	pullPolicy := runetypes.ImagePullAlways
	anonymousPull := false
	if instance.Metadata != nil {
		if instance.Metadata.ImagePull != "" {
			pullPolicy = instance.Metadata.ImagePull
		}
		anonymousPull = instance.Metadata.ImagePullAnonymous
	}
	if err := r.pullImage(ctx, containerConfig.Image, pullPolicy, anonymousPull); err != nil {
		return fmt.Errorf("failed to pull image %s: %w", containerConfig.Image, err)
	}

	resp, name, err := r.createContainerWithUniqueSuffix(ctx, containerConfig, hostConfig, instance)
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	instance.ContainerID = resp.ID

	r.logger.Info("Created container for instance",
		log.Str("container_id", resp.ID),
		log.Str("container_name", name),
		log.Str("instance_id", instance.ID))

	return nil
}

// createContainerWithUniqueSuffix builds a container name of the form
// `<namespace>-<instance.Name>-<id_prefix>` and creates the container,
// extending id_prefix from 8 to 32 hex chars on docker name-conflict. The
// vast majority of calls succeed at 8 chars on the first attempt; only true
// UUID-prefix collisions within the same ns/service/ordinal scope on the
// same host force extension. Capped at 32 (full UUID, no dashes) for safety.
//
// Returns the create response and the final container name actually used.
func (r *DockerRunner) createContainerWithUniqueSuffix(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig, instance *runetypes.Instance) (container.CreateResponse, string, error) {
	prefix := fmt.Sprintf("%s-%s-", instance.Namespace, instance.Name)
	fullID := strings.ReplaceAll(instance.ID, "-", "") // 32 hex chars

	const minSuffix = 8
	for n := minSuffix; n <= len(fullID); n++ {
		name := prefix + fullID[:n]
		resp, err := r.client.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
		if err == nil {
			return resp, name, nil
		}
		if !isNameConflictError(err) {
			return container.CreateResponse{}, "", err
		}
		// Name collision — log once and extend the suffix on next iteration.
		r.logger.Warn("Container name collision; extending suffix",
			log.Str("attempted_name", name),
			log.Str("instance_id", instance.ID),
			log.Int("next_suffix_len", n+1))
	}
	return container.CreateResponse{}, "", fmt.Errorf("name suffix exhausted at %d chars for instance %s (?!)", len(fullID), instance.ID)
}

// isNameConflictError returns true if the error indicates a Docker name conflict
func isNameConflictError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "is already in use by container") || strings.Contains(msg, "Conflict. The container name")
}

// Start starts an existing container.
func (r *DockerRunner) Start(ctx context.Context, instance *runetypes.Instance) error {
	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to get container ID: %w", err)
	}

	// Start the container - using empty StartOptions as we don't need any special configuration
	if err := r.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	// Record the container's IP on its primary network. Endpoint
	// publishing and VIP routing (RUNE-063) key off this — an empty
	// ContainerIP means the service's VIP proxy has no backend and
	// resets every connection. Docker assigns the bridge IP during
	// start, but an inspect issued in the same instant can race ahead
	// of it, so poll briefly rather than inspecting once.
	if instance.Metadata == nil {
		instance.Metadata = &runetypes.InstanceMetadata{}
	}
	if ip := r.waitContainerIP(ctx, containerID); ip != "" {
		instance.Metadata.ContainerIP = ip
	} else {
		r.logger.Warn("Started container but could not determine its IP; endpoints/VIP routing will be degraded",
			log.Str("container_id", containerID),
			log.Str("instance_id", instance.ID))
	}

	r.logger.Info("Started container for instance",
		log.Str("container_id", containerID),
		log.Str("instance_id", instance.ID),
		log.Str("instance_name", instance.Name),
		log.Str("instance_status", string(instance.Status)))

	return nil
}

// Stop stops a running container.
func (r *DockerRunner) Stop(ctx context.Context, instance *runetypes.Instance, timeout time.Duration) error {
	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to get container ID: %w", err)
	}

	// Convert timeout to seconds
	timeoutSeconds := int(timeout.Seconds())

	// Stop the container
	if err := r.client.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeoutSeconds}); err != nil {
		return fmt.Errorf("failed to stop container: %w", err)
	}

	r.logger.Info("Stopped container for instance",
		log.Str("container_id", containerID),
		log.Str("instance_id", instance.ID))

	return nil
}

// Remove removes a container.
func (r *DockerRunner) Remove(ctx context.Context, instance *runetypes.Instance, force bool) error {
	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to get container ID: %w", err)
	}

	// Remove the container
	options := container.RemoveOptions{
		Force: force,
	}

	if err := r.client.ContainerRemove(ctx, containerID, options); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	r.logger.Info("Removed container for instance",
		log.Str("container_id", containerID),
		log.Str("instance_id", instance.ID))

	return nil
}

// Status retrieves the current status of a container.
//
// Self-heal: when the stored instance.ContainerID no longer inspects
// (RUNE-BUG: a stale mapping made the reconciler recreate healthy
// probed services forever), the container is re-resolved through the
// rune.instance.id label — a label match is definitively THIS
// instance's container, since every UUID gets exactly one. On a hit
// the in-memory instance is updated (ContainerID + ContainerIP); the
// orchestrator persists the healed mapping when it observes the
// change. If the label lookup finds nothing the container is
// genuinely gone and the not-found error stands (recreate is correct).
func (r *DockerRunner) Status(ctx context.Context, instance *runetypes.Instance) (runetypes.InstanceStatus, error) {
	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return "", fmt.Errorf("failed to get container ID: %w", err)
	}

	// Get container information
	inspected, err := r.client.ContainerInspect(ctx, containerID)
	if client.IsErrNotFound(err) && instance.ContainerID != "" {
		inspected, err = r.healStaleContainerMapping(ctx, instance, containerID)
	}
	if err != nil {
		return "", fmt.Errorf("failed to inspect container: %w", err)
	}

	// Map Docker state to Rune instance status
	if inspected.State.Running {
		return runetypes.InstanceStatusRunning, nil
	} else if inspected.State.ExitCode != 0 {
		return runetypes.InstanceStatusFailed, nil
	} else {
		return runetypes.InstanceStatusStopped, nil
	}
}

// healStaleContainerMapping re-resolves an instance whose stored
// ContainerID failed to inspect, via the rune.instance.id label. On
// success it rewrites instance.ContainerID and refreshes
// Metadata.ContainerIP from the live container (a stale ID implies the
// recorded IP belongs to the dead container too — probes dialing it
// time out and trigger restart churn). Returns the inspect result of
// the healed container, or the original not-found wrapped with context.
func (r *DockerRunner) healStaleContainerMapping(ctx context.Context, instance *runetypes.Instance, staleID string) (container.InspectResponse, error) {
	liveID, lookupErr := r.lookupContainerByInstanceLabel(ctx, instance)
	if lookupErr != nil {
		return container.InspectResponse{}, fmt.Errorf("stale container ID %s and no labeled replacement: %w", staleID, lookupErr)
	}
	inspected, err := r.client.ContainerInspect(ctx, liveID)
	if err != nil {
		return container.InspectResponse{}, fmt.Errorf("stale container ID %s; labeled container %s failed to inspect: %w", staleID, liveID, err)
	}

	instance.ContainerID = liveID
	if instance.Metadata == nil {
		instance.Metadata = &runetypes.InstanceMetadata{}
	}
	if ip := pickContainerIP(inspected.NetworkSettings); ip != "" {
		instance.Metadata.ContainerIP = ip
	}
	r.logger.Warn("Healed stale container mapping via instance label",
		log.Str("instance_id", instance.ID),
		log.Str("stale_container_id", staleID),
		log.Str("live_container_id", liveID),
		log.Str("container_ip", instance.Metadata.ContainerIP))
	return inspected, nil
}

// List lists all service instances managed by this runner.
func (r *DockerRunner) List(ctx context.Context, namespace string) ([]*runetypes.Instance, error) {
	// Filter containers managed by this runner
	args := filters.NewArgs(
		filters.Arg("label", "rune.managed=true"),
	)

	if namespace != "" {
		args.Add("label", "rune.namespace="+namespace)
	}

	containers, err := r.client.ContainerList(ctx, container.ListOptions{
		All:     true, // Include stopped containers
		Filters: args,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	// Convert Docker API types to Rune types
	instances := make([]*runetypes.Instance, 0, len(containers))

	for _, c := range containers {
		// Extract instance information from container labels
		instanceID := c.Labels["rune.instance.id"]
		if instanceID == "" {
			r.logger.Warn("Found container without instance ID",
				log.Str("container_id", c.ID),
				log.Str("container_name", c.Names[0]))
			continue
		}

		// Create instance object. ServiceName + Namespace are required for
		// the reconciler's orphan detection to match a running container to
		// its service; without ServiceName, replaced containers are never
		// reaped, and without Namespace, a same-named service in a DIFFERENT
		// namespace on this daemon is mistaken for an orphan and its live
		// container reaped (cross-namespace churn — RUNE bug: prod/api and
		// staging/api deleting each other).
		instance := &runetypes.Instance{
			ID:          instanceID,
			ContainerID: c.ID,
			Name:        c.Names[0][1:], // Remove leading slash from container name
			Namespace:   c.Labels["rune.namespace"],
			ServiceID:   c.Labels["rune.service.id"],
			ServiceName: c.Labels["rune.service.name"],
			NodeID:      "local", // Assume local node for now
		}

		// Set status based on container state
		switch c.State {
		case "running":
			instance.Status = runetypes.InstanceStatusRunning
		case "exited":
			// For exited containers, we need to inspect to get the exit code
			inspect, err := r.client.ContainerInspect(ctx, c.ID)
			if err == nil && inspect.State.ExitCode != 0 {
				instance.Status = runetypes.InstanceStatusFailed
			} else {
				instance.Status = runetypes.InstanceStatusStopped
			}
		case "created":
			instance.Status = runetypes.InstanceStatusPending
		default:
			instance.Status = ""
		}

		instances = append(instances, instance)
	}

	return instances, nil
}

// getContainerID gets the container ID for an instance.
//
// Prefers instance.ContainerID when set so failed-instance retention
// tombstones (which share their label-scoped instance.id with a freshly-
// recreated replacement container during the brief window before the GC
// reclaims them) can be addressed unambiguously. Falls back to label
// lookup only on first creation or after a server restart that lost
// the in-memory ContainerID.
func (r *DockerRunner) getContainerID(ctx context.Context, instance *runetypes.Instance) (string, error) {
	if instance.ContainerID != "" {
		return instance.ContainerID, nil
	}
	return r.lookupContainerByInstanceLabel(ctx, instance)
}

// lookupContainerByInstanceLabel resolves an instance's container through
// the rune.instance.id label — the authoritative identity every rune
// container carries. Used on first creation, after a server restart that
// lost the in-memory ContainerID, and by the Status self-heal when a
// stored ContainerID turns out to be stale.
func (r *DockerRunner) lookupContainerByInstanceLabel(ctx context.Context, instance *runetypes.Instance) (string, error) {
	args := filters.NewArgs(
		filters.Arg("label", "rune.managed=true"),
		filters.Arg("label", "rune.instance.id="+instance.ID),
	)

	if instance.Namespace != "" {
		args.Add("label", "rune.namespace="+instance.Namespace)
	}

	containers, err := r.client.ContainerList(ctx, container.ListOptions{
		All:     true, // Include stopped containers
		Filters: args,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list containers: %w", err)
	}

	if len(containers) == 0 {
		return "", fmt.Errorf("no container found for instance ID: %s", instance.ID)
	}

	if len(containers) > 1 {
		r.logger.Warn("Multiple containers found for instance ID",
			log.Str("instance_id", instance.ID),
			log.Int("container_count", len(containers)))
	}

	// Return the first matching container
	return containers[0].ID, nil
}
