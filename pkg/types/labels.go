// Package types: well-known node and resource labels.
//
// These constants are the single source of truth for label keys
// the networking layer reads (RUNE-066 ingress controller, future
// scheduling tickets). Keep them in sync with the operator-facing
// docs.
package types

// Well-known node labels.
const (
	// LabelNodeRole identifies the operational role of a node.
	// The ingress controller (RUNE-066) runs only on nodes whose
	// role contains "edge". Multiple roles are comma-separated.
	LabelNodeRole = "rune.io/role"

	// NodeRoleEdge marks an edge node — one that owns :80/:443
	// and terminates external ingress (HTTP/HTTPS, ACME).
	NodeRoleEdge = "edge"
)

// HasNodeRole reports whether labels carry the given role under
// LabelNodeRole. Empty role or labels return false. Comparison is
// case-sensitive and exact (no substring match) so that "edge-staging"
// does not match "edge".
func HasNodeRole(labels map[string]string, role string) bool {
	if role == "" || len(labels) == 0 {
		return false
	}
	v, ok := labels[LabelNodeRole]
	if !ok || v == "" {
		return false
	}
	// Roles may be comma-separated.
	start := 0
	for i := 0; i <= len(v); i++ {
		if i == len(v) || v[i] == ',' {
			tok := v[start:i]
			// Trim ASCII spaces.
			for len(tok) > 0 && tok[0] == ' ' {
				tok = tok[1:]
			}
			for len(tok) > 0 && tok[len(tok)-1] == ' ' {
				tok = tok[:len(tok)-1]
			}
			if tok == role {
				return true
			}
			start = i + 1
		}
	}
	return false
}

// IsEdgeNode is shorthand for HasNodeRole(labels, NodeRoleEdge).
func IsEdgeNode(labels map[string]string) bool {
	return HasNodeRole(labels, NodeRoleEdge)
}
