//go:build !linux

package upgrade

import "os"

// ResultOwnedByRoot is linux-only in practice (the applier is); other
// platforms never trust a result file.
func ResultOwnedByRoot(os.FileInfo) bool { return false }
