// Package bridges enumerates Docker bridge networks so the agent's
// embedded DNS server can bind on each bridge gateway IP. Containers
// on user-defined networks reach `<service>.<namespace>.rune` through
// their own gateway, so the agent must serve DNS on every bridge
// the host has, not just the default 172.17.0.1.
//
// The implementation is a small, dependency-light helper around the
// Docker SDK's NetworkList / NetworkInspect. Errors from the Docker
// daemon are surfaced to the caller so they can decide whether to
// log-and-continue or hard-fail (the agent typically does the former
// because the loopback bind on 127.0.0.123 is sufficient for the
// no-Docker dev path).
package bridges

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// Lister is satisfied by *client.Client; defined as an interface so
// callers can inject a fake in tests.
type Lister interface {
	NetworkList(ctx context.Context, options network.ListOptions) ([]network.Inspect, error)
	NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error)
}

// Gateway pairs a Docker network's name with one of its bridge
// gateway IPs.
type Gateway struct {
	NetworkID   string
	NetworkName string
	IP          net.IP
}

// String returns "<networkName>=<ip>".
func (g Gateway) String() string {
	return fmt.Sprintf("%s=%s", g.NetworkName, g.IP)
}

// EnumerateGateways returns the IPv4 gateway address of every Docker
// network with driver "bridge". The default `bridge` network is
// included alongside any user-defined networks (e.g. those created
// by docker-compose).
func EnumerateGateways(ctx context.Context, c Lister) ([]Gateway, error) {
	nets, err := c.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("bridges: list networks: %w", err)
	}
	var out []Gateway
	for _, n := range nets {
		if !strings.EqualFold(n.Driver, "bridge") {
			continue
		}
		// NetworkList may not populate IPAM for every network depending
		// on Docker version; an Inspect call always does.
		insp, err := c.NetworkInspect(ctx, n.ID, network.InspectOptions{})
		if err != nil {
			// Skip and continue; the agent prefers to bind on the
			// networks it can rather than fail the whole subsystem.
			continue
		}
		for _, cfg := range insp.IPAM.Config {
			if cfg.Gateway == "" {
				continue
			}
			ip := net.ParseIP(cfg.Gateway)
			if ip == nil {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				out = append(out, Gateway{
					NetworkID:   insp.ID,
					NetworkName: insp.Name,
					IP:          v4,
				})
			}
		}
	}
	return out, nil
}

// NewClient is a thin wrapper around client.NewClientWithOpts(client.FromEnv)
// for callers that don't already have a Docker client. Returned
// client should be Closed by the caller.
func NewClient() (*client.Client, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("bridges: docker client: %w", err)
	}
	return c, nil
}
