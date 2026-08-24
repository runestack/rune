package startup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/log"
)

// The RUNE-313 ordering guards.
//
// The design's headline failure mode is a refactor that moves node wiring
// earlier: reading node identity before the agent has started, or hoisting the
// real mount resolver out of subsystem registration (which runs BEFORE
// agent.Start) into the post-start wiring phase. An earlier draft of the RFC
// proposed exactly the latter.
//
// Neither is caught by the e2e harness — it never runs an edge node and its
// DNS coverage is host-dependent — and neither is expressible in the type
// system: the package boundary stops outside callers, but inside the package
// nothing orders one phase against another. So they are asserted here.
//
// Both assertions below were rebuilt after a review demonstrated that their
// first versions passed even when the bugs they name were introduced: the
// panic test recovered a nil-deref instead of the guard, and the resolver test
// checked file membership, which cannot see WHERE in a file a call sits.

// TestWireNodeEndpoints_RequiresStartedNode pins the runtime guard. It asserts
// the recovered value is the guard's own message: a bare `recover() != nil`
// passes on any nil dereference, which is precisely how the first version of
// this test survived deletion of the guard.
func TestWireNodeEndpoints_RequiresStartedNode(t *testing.T) {
	cases := []struct {
		name string
		n    *node
	}{
		{"nil node", nil},
		{"node that has not started", &node{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("wireNodeEndpoints must panic when the agent has not started: " +
						"reading node identity before agent.Start is the ordering bug RUNE-313 exists to prevent")
				}
				msg, ok := r.(string)
				if !ok || !strings.Contains(msg, "before the agent started") {
					t.Fatalf("panicked for the wrong reason (%v): the guard must reject this, "+
						"not a downstream nil dereference — a nil-deref panic means the guard is gone", r)
				}
			}()
			// A real logger, so the function cannot panic incidentally on a nil
			// logger and thereby hide a missing guard.
			wireNodeEndpoints(&boot{logger: log.NewLogger()}, &controlPlane{}, tc.n)
		})
	}
}

// TestMountResolverWiredInsideRegistration asserts the POSITION of the
// SetMountResolver call, not merely which file it lives in: it must sit
// lexically inside the registration closure passed to startAgent, because
// startAgent invokes that closure before agent.Start. Hoisting it anywhere
// after startAgent returns delays the swap away from notReadyMountResolver and
// widens the window in which every volume reports "not yet mounted".
func TestMountResolverWiredInsideRegistration(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "node.go", nil, 0)
	if err != nil {
		t.Fatalf("parse node.go: %v", err)
	}

	// Locate the registration closure: the func literal argument of startAgent.
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
			"if the shape changed, this guard must be rewritten, not deleted — it is the " +
			"only check that the mount resolver is wired before agent.Start")
	}

	inside, outside := findCalls(file, "SetMountResolver", regClosure)
	if inside == 0 {
		t.Error("SetMountResolver must be called INSIDE the registration closure passed to " +
			"startAgent: that closure runs before agent.Start, which is what makes the swap " +
			"from notReadyMountResolver happen as early as possible (RUNE-313 D2)")
	}
	for _, pos := range outside {
		t.Errorf("SetMountResolver called outside the registration closure at %s: "+
			"anything after startAgent returns runs after agent.Start and widens the "+
			"not-yet-mounted window (RUNE-313 D2)", fset.Position(pos))
	}

	// The publisher is the opposite constraint: it must NOT be in
	// registration.
	//
	// STALE RATIONALE, FLAGGED NOT REWRITTEN: this guard was justified by
	// "it needs node identity, which does not exist until the agent has
	// started". That is no longer true — the identity load now happens
	// before the store opens, and NewEndpointPublisher takes only the
	// OrderedLog, which is already open by then. Whatever still pins the
	// call to after agent.Start (op-kind registration order against the
	// DNS subsystem is the likely candidate, see wiring.go's comment) has
	// not been established here, so the assertion is left exactly as it
	// was rather than re-justified on a guess. Establish the real
	// constraint before relaxing or rewording this.
	pubInside, _ := findCalls(file, "SetEndpointPublisher", regClosure)
	if pubInside > 0 {
		t.Error("SetEndpointPublisher must NOT be called inside the registration closure; " +
			"see the note above — the original identity-based rationale is stale but the " +
			"constraint has not been shown to be")
	}
}

// findCalls reports how many calls to the named selector sit inside the given
// closure, and the positions of any that sit outside it.
func findCalls(file *ast.File, selector string, within *ast.FuncLit) (inside int, outside []token.Pos) {
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != selector {
			return true
		}
		if call.Pos() >= within.Pos() && call.End() <= within.End() {
			inside++
		} else {
			outside = append(outside, call.Pos())
		}
		return true
	})
	return inside, outside
}
