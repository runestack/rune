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
// silently loses its probe line. That is the same shape as the blank
// imports whose deletion shipped an empty driver registry in dev.145 —
// an absent wiring call is not a compile error, and no other gate sees it
// (see .claude/BRANCH_TEST.md, "What none of these layers can see").
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

	// nodeinfo.New(...) constructs it ...
	var constructed bool
	ast.Inspect(regClosure, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "nodeinfo" && sel.Sel.Name == "New" {
			constructed = true
		}
		return true
	})
	if !constructed {
		t.Error("nodeinfo.New must be called inside the registration closure: without it no " +
			"node record is ever written, and `rune describe node` reports the node as " +
			"absent on a perfectly healthy box")
	}

	// ... and a.Register(...) hands it to the agent. Counted rather than
	// matched by argument name so a rename of the local does not silently
	// disable the guard.
	registers, _ := findCalls(file, "Register", regClosure)
	if registers < 1 {
		t.Error("the inventory subsystem must be handed to agent.Register inside the " +
			"registration closure; constructing it and not registering it is a silent no-op")
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
