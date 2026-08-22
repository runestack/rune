// Container IP discovery for endpoint publishing. Split from runner.go
// (RUNE-312).

package docker

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"
	runetypes "github.com/runestack/rune/pkg/types"
)

// waitContainerIP polls the container inspect until its primary
// network reports an IP, the container has exited, or the budget
// expires. A single inspect issued right after ContainerStart can race
// the bridge IP assignment and return "", which left instances with an
// empty ContainerIP — breaking endpoint publishing and VIP routing.
func (r *DockerRunner) waitContainerIP(ctx context.Context, containerID string) string {
	const (
		budget   = 5 * time.Second
		interval = 100 * time.Millisecond
	)
	deadline := time.Now().Add(budget)
	for {
		if insp, err := r.client.ContainerInspect(ctx, containerID); err == nil {
			if insp.NetworkSettings != nil {
				if ip := pickContainerIP(insp.NetworkSettings); ip != "" {
					return ip
				}
			}
			// A container that has already exited will never get an IP.
			if insp.State != nil && !insp.State.Running {
				return ""
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(interval):
		}
	}
}

// InstanceIP implements runner.IPProvider for endpoint publishing.
func (r *DockerRunner) InstanceIP(ctx context.Context, instance *runetypes.Instance) (string, error) {
	containerID, err := r.getContainerID(ctx, instance)
	if err != nil {
		return "", err
	}
	insp, err := r.client.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}
	if ip := pickContainerIP(insp.NetworkSettings); ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("container has no IPv4 address")
}

// pickContainerIP returns the container's primary IPv4 address from
// an inspect result. Prefers the per-network EndpointSettings.IPAddress
// (works for user-defined networks too), and falls back to the legacy
// DefaultNetworkSettings.IPAddress for the default bridge.
func pickContainerIP(ns *container.NetworkSettings) string {
	if ns == nil {
		return ""
	}
	for _, ep := range ns.Networks {
		if ep != nil && ep.IPAddress != "" {
			return ep.IPAddress
		}
	}
	return ns.DefaultNetworkSettings.IPAddress
}
