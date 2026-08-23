package startup

import (
	"testing"

	"github.com/runestack/rune/pkg/storage/driver"
)

// TestBuiltinStorageDriversRegistered pins the blank imports in node.go.
//
// They exist only for their registration side effect, so every tool that
// reasons about "unused" imports — goimports especially — will happily delete
// them the next time this code moves. RUNE-313 did exactly that while pulling
// startup phases out of main.go, and v0.0.1-dev.145 shipped with an empty
// driver registry: any volume needing its mount re-resolved failed with
// `handle not found: "do-volume"`, taking a production database down on
// restart. Nothing else in the tree references these packages by name, so
// this test is the only thing standing between a stray goimports run and a
// silent repeat.
func TestBuiltinStorageDriversRegistered(t *testing.T) {
	for _, name := range []string{"local", "local-host", "do-volume", "aws-ebs", "gce-pd", "hcloud-volume"} {
		if _, ok := driver.Lookup(name); !ok {
			t.Errorf("storage driver %q is not registered.\n"+
				"A blank import in node.go was probably removed as 'unused'. "+
				"They are load-bearing: without them the registry is empty and "+
				"every volume mount fails with 'handle not found'.", name)
		}
	}
}
