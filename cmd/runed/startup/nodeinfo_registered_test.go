package startup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The node inventory subsystem is wired by a single call in the
// registration closure, and nothing downstream fails if that call goes
// away: `runed` boots fine, every command works, and the only symptom is
// that `rune describe node` says the node does not exist and `rune status`
// silently loses its probe line. An absent wiring call is not a compile
// error, no test that does not look for it will notice, and the same
// shape once shipped a release whose storage-driver registry was empty
// because a refactor dropped the blank imports that populated it.
//
// Asserted at the AST level for the same reason the mount-resolver guard
// is: file membership cannot see WHERE a call sits, and this one has to
// be inside the closure startAgent invokes.
func TestNodeInfoSubsystemRegistered(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "node.go", nil, 0)
	if err != nil {
		t.Fatalf("parse node.go: %v", err)
	}

	var regClosure *ast.FuncLit
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "startAgent" {
			return true
		}
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.FuncLit); ok {
				regClosure = lit
			}
		}
		return false
	})
	if regClosure == nil {
		t.Fatal("could not find the registration closure passed to startAgent in node.go; " +
			"if the shape changed, rewrite this guard rather than deleting it")
	}

	// nodeinfo.New(...) constructs it, and we remember what it was
	// assigned to so the Register check below can be specific.
	constructedAs := assignedFromCall(regClosure, "nodeinfo", "New")
	if constructedAs == "" {
		t.Fatal("nodeinfo.New must be called inside the registration closure and its result " +
			"assigned: without it no node record is ever written, and `rune describe node` " +
			"reports the node as absent on a perfectly healthy box")
	}

	// ... and a.Register(<that value>) hands it to the agent. Matched on
	// the identifier nodeinfo.New was assigned to, NOT counted: there are
	// half a dozen other Register calls in this closure, so a count can
	// never drop below one and would pass with the inventory
	// registration deleted — the exact failure this guard exists for.
	if !registersIdent(regClosure, constructedAs) {
		t.Errorf("the value from nodeinfo.New (%q) must be passed to agent.Register inside "+
			"the registration closure; constructing a subsystem and not registering it is a "+
			"silent no-op", constructedAs)
	}
}

// The provider is selected from the flag, not hardcoded. A hardcoded
// NVIDIASMIProvider would fork nvidia-smi on every GPU-less machine in
// the fleet and take away `--gpu-provider=none`, the operator's
// guarantee that nothing is probed.
func TestGPUProviderComesFromTheFlag(t *testing.T) {
	src, err := parser.ParseFile(token.NewFileSet(), "node.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse node.go: %v", err)
	}
	var found bool
	ast.Inspect(src, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SelectProvider" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "nodeinfo" {
			return true
		}
		for _, arg := range call.Args {
			if s, ok := arg.(*ast.SelectorExpr); ok && strings.Contains(s.Sel.Name, "GPUProvider") {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Error("nodeinfo.SelectProvider must be called with flags.GPUProvider: hardcoding a " +
			"provider forks a probe on every GPU-less machine and removes --gpu-provider=none")
	}
}

// assignedFromCall returns the name the result of pkg.Fn(...) was assigned
// to inside fn, or "" when there is no such call. Only the first result is
// considered; a second (an error) is ignored.
func assignedFromCall(fn *ast.FuncLit, pkg, name string) string {
	var found string
	ast.Inspect(fn, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != pkg {
			return true
		}
		if id, ok := assign.Lhs[0].(*ast.Ident); ok {
			found = id.Name
		}
		return false
	})
	return found
}

// registersIdent reports whether fn contains a call to .Register(ident).
func registersIdent(fn *ast.FuncLit, ident string) bool {
	var found bool
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Register" || len(call.Args) != 1 {
			return true
		}
		if id, ok := call.Args[0].(*ast.Ident); ok && id.Name == ident {
			found = true
		}
		return true
	})
	return found
}
