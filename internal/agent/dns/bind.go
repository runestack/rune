package dns

import (
	"net"

	"github.com/runestack/rune/pkg/log"
)

// DevDefaultBindAddr is the loopback fallback for laptop dev (macOS cannot
// bind 127.0.0.123 without an interface alias, and port 53 requires root).
// Host-side .rune access in dev mode goes through the dataplane on 127.0.0.1;
// this address keeps the embedded resolver available for dig/host testing.
const DevDefaultBindAddr = "127.0.0.1:15353"

// FilterBindable returns host:port pairs where both UDP and TCP listeners
// can be opened. Docker Desktop on macOS reports bridge gateways such as
// 172.17.0.1 that are not assigned to the host network namespace; probing
// here avoids failing the whole agent when those addresses are unreachable.
func FilterBindable(addrs []string, logger log.Logger) []string {
	seen := make(map[string]struct{}, len(addrs))
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if addr == "" {
			continue
		}
		if _, dup := seen[addr]; dup {
			continue
		}
		seen[addr] = struct{}{}
		if probeBind(addr) {
			out = append(out, addr)
		} else if logger != nil {
			logger.Debug("dns: skipping non-bindable address", log.Str("addr", addr))
		}
	}
	return out
}

func probeBind(addr string) bool {
	uc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return false
	}
	_ = uc.Close()
	tc, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = tc.Close()
	return true
}
