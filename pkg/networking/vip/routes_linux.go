//go:build linux

package vip

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// checkRouteCollision returns an error if any existing host route
// overlaps the supplied IPNet. Used at Bootstrap to catch the
// "configured 10.96.0.0/16 but the host already has a 10.96.0.0/24
// route from another VPN" mistake before it becomes a routing
// black hole.
func checkRouteCollision(ipnet *net.IPNet) error {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		// Don't refuse to start over a netlink failure; log via the
		// caller and continue. Returning an error here would block
		// startup on container hosts where netlink may be restricted.
		return nil
	}
	for _, r := range routes {
		if r.Dst == nil {
			continue
		}
		if cidrsOverlap(r.Dst, ipnet) {
			return fmt.Errorf("existing route %s overlaps cluster CIDR %s", r.Dst.String(), ipnet.String())
		}
	}
	return nil
}

func cidrsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}
