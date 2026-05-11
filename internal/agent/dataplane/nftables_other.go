//go:build !linux

package dataplane

import "github.com/runestack/rune/pkg/log"

// newNFTables returns a no-op manager on non-Linux platforms.
// nftables is Linux-only; dev-mode users on macOS rely on the proxy's
// 127.0.0.1 listener path (see proxy.bindIPFor).
func newNFTables(_ Mode, logger log.Logger) nftablesManager {
	return &noopNftables{log: logger}
}
