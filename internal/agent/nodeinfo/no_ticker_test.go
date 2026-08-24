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
	const why = "a recurring wakeup on every GPU-less machine in the fleet"
	forbidden := map[string]string{
		"time.NewTicker": why,
		"time.Tick":      why,
		// time.After is fine once (probe() would use it); inside a loop
		// it is a ticker spelled differently, and spelling is not a
		// distinction the fleet can feel. Checked by the loop scan below
		// rather than banned outright.
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
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if ident, ok := sel.X.(*ast.Ident); ok {
						name := ident.Name + "." + sel.Sel.Name
						if why, bad := forbidden[name]; bad {
							t.Errorf("%s uses %s at %s — %s",
								path, name, fset.Position(sel.Pos()), why)
						}
					}
				}
				// A time.After (or a bare sleep) inside a loop is a ticker
				// spelled differently. Banning the constructor alone would
				// leave the obvious workaround open.
				if body := loopBody(n); body != nil {
					ast.Inspect(body, func(inner ast.Node) bool {
						sel, ok := inner.(*ast.SelectorExpr)
						if !ok {
							return true
						}
						ident, ok := sel.X.(*ast.Ident)
						if !ok || ident.Name != "time" {
							return true
						}
						if sel.Sel.Name == "After" || sel.Sel.Name == "Sleep" || sel.Sel.Name == "Tick" {
							t.Errorf("%s waits with time.%s inside a loop at %s — %s",
								path, sel.Sel.Name, fset.Position(sel.Pos()), why)
						}
						return true
					})
				}
				return true
			})
		}
	}
}

// loopBody returns the body of n when n is a loop, else nil.
func loopBody(n ast.Node) ast.Node {
	switch l := n.(type) {
	case *ast.ForStmt:
		return l.Body
	case *ast.RangeStmt:
		return l.Body
	}
	return nil
}
