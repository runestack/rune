package nodeinfo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// A GPU-less box must get no recurring wakeup.
// TestSubsystem_NothingPeriodicOnAGPULessBox asserts that at runtime;
// this asserts it at the source level, because the failure mode is not a
// bug someone hits — it is a ticker someone adds on purpose "so it picks
// up a card later". Hotplug is out of scope; a re-probe is a restart.
//
// If a periodic re-probe is ever genuinely needed, that is a deliberate
// decision about what runs on every machine in the fleet, not a detail of
// a patch. Delete this test on purpose, with that argument written down.
func TestNoPeriodicTimersInPackage(t *testing.T) {
	forbidden := map[string]string{
		"time.NewTicker": "a ticker is a recurring wakeup on every GPU-less machine in the fleet",
		"time.Tick":      "a ticker is a recurring wakeup on every GPU-less machine in the fleet",
	}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				name := ident.Name + "." + sel.Sel.Name
				if why, bad := forbidden[name]; bad {
					t.Errorf("%s uses %s at %s — %s",
						path, name, fset.Position(sel.Pos()), why)
				}
				return true
			})
		}
	}
}
