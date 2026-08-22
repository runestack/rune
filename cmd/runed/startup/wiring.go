package startup

import (
	"net"
	"strings"

	dnssub "github.com/runestack/rune/internal/agent/dns"

	"github.com/runestack/rune/pkg/log"
)

// wireNodeEndpoints runs startup phase 8 (RUNE-313): the two bindings that
// genuinely cannot happen before the agent is running.
//
// It carries the endpoint publisher (needs node identity) and container DNS
// injection (needs the DNS subsystem's bound addresses). It deliberately does
// NOT carry the mount resolver — that one must precede agent.Start and lives
// in the volumes registration in startup_node.go. An earlier draft of
// RUNE-313 moved it here; review caught that it would widen the very
// not-yet-mounted window the design exists to protect.
func wireNodeEndpoints(b *boot, cp *controlPlane, n *node) {
	if n == nil || !n.started {
		// Package-main types cannot express "after mustStartNode", so this
		// is the guard that actually enforces it. Panics rather than
		// mis-wires: reading node identity before agent.Start is the
		// ordering bug this phase exists to make impossible.
		panic("wireNodeEndpoints called before the agent started (RUNE-313 ordering constraint)")
	}

	logger := b.logger
	olog := cp.olog
	apiServer := cp.api
	agentInst := n.agent
	dnsSub := n.dns

	// Wire the orchestrator's instance controller to the OrderedLog-
	// backed networking publishers (RUNE-063). Best-effort: if either
	// op-kind has already been registered (dnsSub.New just did so via
	// the agent.Register path), Register is idempotent.
	if pub, perr := dnssub.NewEndpointPublisher(olog, logger.WithComponent("endpoint-publisher")); perr != nil {
		logger.Warn("Endpoint publisher disabled", log.Err(perr))
	} else {
		apiServer.GetOrchestrator().SetEndpointPublisher(pub, agentInst.Identity().NodeID)
	}

	// Tell the docker runner to inject Rune's embedded DNS server
	// into every subsequently-created container (RUNE-063). Without
	// this call, containers inherit the host's /etc/resolv.conf and
	// cannot resolve `<service>.<namespace>.rune` — every
	// cross-service hostname returns NXDOMAIN from upstream DNS.
	// We strip the :port suffix because docker's --dns expects
	// addresses only (it always queries on UDP/TCP 53), and the
	// search domain ensures bare service names ("mongo") resolve
	// inside the same namespace.
	if dnsSub != nil {
		dnsIPs := dnsServerIPs(dnsSub.BindAddrs())
		if len(dnsIPs) > 0 {
			apiServer.GetRunnerManager().SetDNSInjection(dnsIPs, []string{"rune."})
			logger.Info("Container DNS injection enabled",
				log.Str("servers", strings.Join(dnsIPs, ",")))
		} else {
			logger.Warn("DNS subsystem has no bind addresses; container DNS injection skipped")
		}
	}

}

// dnsServerIPs strips the :port suffix from each "host:port" bind
// address and returns the container-reachable IPs in stable order.
// docker's --dns flag wants addresses, not host:port pairs (it always
// queries on 53).
//
// Loopback addresses (e.g. the 127.0.0.123 host bind) are dropped:
// inside a container 127.x.x.x is the container's *own* loopback, not
// the host's, so injecting one as a nameserver gives every container a
// dead resolver entry — lookups hit it first and intermittently fail
// with ENOTFOUND. Only bridge-gateway addresses are reachable from a
// container network namespace.
func dnsServerIPs(bindAddrs []string) []string {
	out := make([]string, 0, len(bindAddrs))
	seen := make(map[string]struct{}, len(bindAddrs))
	for _, addr := range bindAddrs {
		host, _, err := net.SplitHostPort(addr)
		if err != nil || host == "" {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}
