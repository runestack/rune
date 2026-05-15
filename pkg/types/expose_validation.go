// Package types: ingress / expose validation.
//
// ValidateExpose enforces the pre-cast invariants required by the
// ingress controller:
//
//   - acme TLS requires a non-empty Host (the ACME provider needs
//     a hostname to issue against).
//   - Path, when set, must start with "/".
//
// This is a value-shape check; it does not require any cluster state
// and is safe to call from cast pipelines and admission tests.
package types

import "strings"

// ValidateExpose checks a ServiceExpose value for the invariants
// required by the ingress controller. Returns nil if e is nil.
//
// onEdge is reserved for future per-edge checks (currently unused;
// kept in the signature for API stability).
func ValidateExpose(e *ServiceExpose, onEdge bool) error {
	_ = onEdge
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
	if e.TLS != nil && e.TLS.Mode == ExposeTLSModeManual {
		if e.TLS.SecretName == "" {
			return NewValidationError("expose.tls.secretName is required when expose.tls.mode is manual")
		}
		if e.Host == "" {
			return NewValidationError("expose.host is required when expose.tls.mode is manual")
		}
	}
	return nil
}
