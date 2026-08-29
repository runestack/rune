//go:build linux

package upgrade

import (
	"os"
	"syscall"
)

// ResultOwnedByRoot reports whether the result file is root-owned.
// Defense-in-depth: the 0755 root workdir already blocks non-root writes,
// but the event the result feeds is the upgrade audit trail, so ownership
// is checked before the content is believed.
func ResultOwnedByRoot(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0
}
