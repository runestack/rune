package probes

import (
	"fmt"
	"net"
	"time"
)

// TCPProber implements the TCP health check probe
type TCPProber struct{}

// Execute implements the Prober interface for TCP probes
func (p *TCPProber) Execute(ctx *ProbeContext) ProbeResult {
	start := time.Now()

	// Container instances are dialled by their container IP so the
	// probe bypasses the host's ingress listener. An explicit `host:`
	// on the probe overrides that — see probeHost.
	host := probeHost(ctx.ProbeConfig, ctx.Instance)

	// Attempt to establish TCP connection
	dialer := net.Dialer{Timeout: probeTimeout(ctx.ProbeConfig)}
	conn, err := dialer.DialContext(ctx.Ctx, "tcp", fmt.Sprintf("%s:%d", host, ctx.ProbeConfig.Port))
	if err != nil {
		return ProbeResult{
			Success:  false,
			Message:  fmt.Sprintf("TCP health check failed: %v", err),
			Duration: time.Since(start),
		}
	}
	defer conn.Close()

	return ProbeResult{
		Success:  true,
		Message:  "TCP health check succeeded",
		Duration: time.Since(start),
	}
}
