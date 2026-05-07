// Package types: ingress / expose validation.
//
// ValidateExpose enforces the pre-cast invariants required by the
// ingress controller:
//
//   - acme TLS requires a non-empty Host (the ACME provider needs
//     a hostname to issue against).
//   - Expose.HostPort cannot be 80 or 443 on edge nodes; those
//     ports are owned exclusively by the ingress listener and
//     would create rule-precedence ambiguity with a service VIP
//     binding the same port.
//   - Path, when set, must start with "/".
//
// This is a value-shape check; it does not require any cluster state
// and is safe to call from cast pipelines and admission tests.
package types

import "strings"

// EdgeReservedPorts are the host ports the ingress controller owns
// on edge nodes. Pre-cast validation rejects services configured
// to bind these.
var EdgeReservedPorts = []int{80, 443}

// ValidateExpose checks a ServiceExpose value for the invariants
// required by the ingress controller. Returns nil if e is nil.
//
// onEdge is true when the cast is targeting (or might land on) an
// edge node. Single-node deployments where the only node is an edge
// node should pass true. The check that depends on onEdge is the
// reserved-port collision; all other checks always run.
func ValidateExpose(e *ServiceExpose, onEdge bool) error {
	if e == nil {
		return nil
	}
	if e.Path != "" && !strings.HasPrefix(e.Path, "/") {
		return NewValidationError("expose.path must start with '/'")
	}
	if e.TLS != nil && e.TLS.IsACME() {
		if e.Host == "" {
			return NewValidationError("expose.host is required when expose.tls.mode is acme")
		}
	}
	if onEdge {
		for _, p := range EdgeReservedPorts {
			if e.HostPort == p {
				return NewValidationError("expose.hostPort " +
					itoaPort(p) +
					" is reserved by the ingress controller on edge nodes")
			}
		}
	}
	return nil
}

func itoaPort(p int) string {
	switch p {
	case 80:
		return "80"
	case 443:
		return "443"
	default:
		// Small int; stringify without importing strconv to keep
		// this file dependency-free.
		var b [4]byte
		i := len(b)
		n := p
		if n == 0 {
			return "0"
		}
		for n > 0 {
			i--
			b[i] = byte('0' + n%10)
			n /= 10
		}
		return string(b[i:])
	}
}
