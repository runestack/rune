//go:build linux

package dataplane

import (
	"fmt"
	"net"
	"sync"

	"github.com/vishvananda/netlink"
)

// localVIPHost adds/removes service VIP /32 addresses on loopback so
// production-mode proxy listeners can bind and receive traffic from
// containers via the docker bridge gateway.
type localVIPHost struct {
	mu   sync.Mutex
	refs map[string]int // VIP string -> refcount
}

func newLocalVIPHost() *localVIPHost {
	return &localVIPHost{refs: make(map[string]int)}
}

func (h *localVIPHost) add(ip net.IP) error {
	if ip == nil {
		return nil
	}
	key := ip.String()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.refs[key] > 0 {
		h.refs[key]++
		return nil
	}
	if err := addLoopbackVIP(ip); err != nil {
		return err
	}
	h.refs[key] = 1
	return nil
}

func (h *localVIPHost) remove(ip net.IP) {
	if ip == nil {
		return
	}
	key := ip.String()
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.refs[key]
	if n <= 1 {
		delete(h.refs, key)
		_ = removeLoopbackVIP(ip)
		return
	}
	h.refs[key] = n - 1
}

func addLoopbackVIP(ip net.IP) error {
	link, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("loopback link: %w", err)
	}
	addr, err := netlink.ParseAddr(ip.String() + "/32")
	if err != nil {
		return fmt.Errorf("parse VIP addr: %w", err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		if isAddrExists(err) {
			return nil
		}
		return fmt.Errorf("addr add %s: %w", ip, err)
	}
	return nil
}

func removeLoopbackVIP(ip net.IP) error {
	link, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	addr, err := netlink.ParseAddr(ip.String() + "/32")
	if err != nil {
		return err
	}
	if err := netlink.AddrDel(link, addr); err != nil {
		if isAddrNotPresent(err) {
			return nil
		}
		return err
	}
	return nil
}

func isAddrExists(err error) bool {
	return err != nil && containsAny(err.Error(), "file exists", "address already assigned", "EEXIST")
}

func isAddrNotPresent(err error) bool {
	return err != nil && containsAny(err.Error(), "cannot assign", "not found", "no such process")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
