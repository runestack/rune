//go:build linux

package dataplane

import (
	"os"
	"strings"

	"github.com/runestack/rune/pkg/log"
)

// ensureNonLocalBind enables binding to cluster VIPs before their /32
// is on loopback. It is belt-and-suspenders against a bind/listen race
// only — it does NOT route inbound VIP traffic. Delivery still depends
// on the loopback /32 added by vip_host_linux.go.
func ensureNonLocalBind(logger log.Logger) {
	const path = "/proc/sys/net/ipv4/ip_nonlocal_bind"
	data, err := os.ReadFile(path)
	if err != nil {
		logger.Warn("dataplane: cannot read ip_nonlocal_bind", log.Err(err))
		return
	}
	if strings.TrimSpace(string(data)) == "1" {
		return
	}
	if err := os.WriteFile(path, []byte("1"), 0o644); err != nil {
		logger.Warn("dataplane: set ip_nonlocal_bind=1 failed (run as root or set sysctl); VIP listeners may not bind",
			log.Err(err))
		return
	}
	logger.Info("dataplane: enabled net.ipv4.ip_nonlocal_bind for cluster VIP listeners")
}
