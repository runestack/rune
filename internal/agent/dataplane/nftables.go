package dataplane

import "github.com/runestack/rune/pkg/log"

// nftablesManager is the dataplane's interface to the kernel-side
// rule programmer. Production-Linux implementations reconcile the
// `rune` nftables table; non-Linux and dev-mode use a no-op.
//
// The full DNAT-programming implementation is intentionally deferred:
// the userspace proxy already provides correctness end-to-end (clients
// connect to the VIP — see proxy.go bindIPFor), so nftables is a
// performance optimization that can be staged in a follow-up ticket
// without behavioral change.
type nftablesManager interface {
	// Close releases all programmed rules. Idempotent.
	Close() error
}

// noopNftables is used in dev mode and on non-Linux platforms.
type noopNftables struct{ log log.Logger }

func (n *noopNftables) Close() error { return nil }
