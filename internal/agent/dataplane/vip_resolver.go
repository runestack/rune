package dataplane

import (
	"context"
	"fmt"
	"net"

	"github.com/runestack/rune/pkg/types"
)

// VIPResolver resolves the stable cluster VIP for a service ID.
type VIPResolver interface {
	VIPForService(ctx context.Context, serviceID string) (net.IP, error)
}

// FuncVIPResolver adapts a lookup function to VIPResolver.
type FuncVIPResolver struct {
	Fn func(ctx context.Context, serviceID string) (net.IP, error)
}

func (f FuncVIPResolver) VIPForService(ctx context.Context, serviceID string) (net.IP, error) {
	if f.Fn == nil {
		return nil, fmt.Errorf("dataplane: nil VIP resolver")
	}
	return f.Fn(ctx, serviceID)
}

// enrichServiceVIP ensures svc.Discovery.VIP is set, consulting the
// allocator when the store row is missing it (legacy drift).
func enrichServiceVIP(ctx context.Context, svc *types.Service, vip VIPResolver) error {
	if svc == nil || svc.ID == "" {
		return nil
	}
	if svc.Discovery != nil && svc.Discovery.VIP != "" {
		return nil
	}
	if vip == nil {
		return fmt.Errorf("dataplane: service %s/%s has no VIP and no resolver configured", svc.Namespace, svc.Name)
	}
	ip, err := vip.VIPForService(ctx, svc.ID)
	if err != nil {
		return fmt.Errorf("dataplane: resolve VIP for %s: %w", svc.ID, err)
	}
	if ip == nil {
		return fmt.Errorf("dataplane: nil VIP for service %s", svc.ID)
	}
	if svc.Discovery == nil {
		svc.Discovery = &types.ServiceDiscovery{}
	}
	svc.Discovery.VIP = ip.String()
	return nil
}
