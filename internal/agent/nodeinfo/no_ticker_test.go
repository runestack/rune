package nodeinfo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// RUNE-301 §12.4(a) promises that a GPU-less box gets no recurring
// wakeup. TestSubsystem_NothingPeriodicOnAGPULessBox asserts that at
// runtime; this asserts it at the source level, because the failure the
// design names is not a bug an implementer hits — it is a ticker someone
// adds on purpose "so it picks up a card later", and hotplug is a
// non-goal. A re-probe is a restart.
//
// If a future slice genuinely needs a periodic re-probe, that is a change
// to the design, not a detail of a patch: update RUNE-301 first, then
// this test.
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
					t.Errorf("%s uses %s at %s — %s (RUNE-301 §12.4a)",
						path, name, fset.Position(sel.Pos()), why)
				}
				return true
			})
		}
	}
}
