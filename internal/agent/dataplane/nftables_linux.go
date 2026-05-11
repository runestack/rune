//go:build linux

package dataplane

import "github.com/runestack/rune/pkg/log"

// newNFTables returns a manager that programs nftables rules on Linux.
// The current implementation is the no-op variant; the production
// reconciler that owns table `rune` will land in a follow-up ticket
// once we have an integration sandbox to exercise it. Behavior on the
// fast path is unchanged because the userspace proxy already binds
// to the VIP itself (see proxy.bindIPFor).
func newNFTables(mode Mode, logger log.Logger) nftablesManager {
	if mode == ModeDev {
		return &noopNftables{log: logger}
	}
	logger.Warn("dataplane: nftables programming is currently a no-op (deferred); userspace proxy binds VIP directly")
	return &noopNftables{log: logger}
}
