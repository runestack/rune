package main

import (
	"testing"
)

// The RUNE-313 ordering guard. The design's headline failure mode is a
// refactor that moves node wiring earlier — reading node identity before the
// agent has started, or hoisting the mount resolver out of subsystem
// registration. Neither is expressible in the type system (every phase is in
// package main, so a nil *node compiles), and neither is caught by the e2e
// harness, which never runs an edge node and whose DNS coverage is
// host-dependent. So it is asserted here.

// TestWireNodeEndpoints_RequiresStartedNode pins the guard that stands in for
// the compile-time ordering constraint we cannot express.
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
				if recover() == nil {
					t.Fatal("wireNodeEndpoints must panic when the agent has not started: " +
						"reading node identity before agent.Start is the ordering bug RUNE-313 exists to prevent")
				}
			}()
			wireNodeEndpoints(&boot{}, &controlPlane{}, tc.n)
		})
	}
}

// TestMountResolverWiredBeforeAgentStart is the source-level assertion that
// the real mount resolver stays inside subsystem registration (which runs
// before agent.Start) rather than migrating into the post-start wiring phase.
// An earlier draft of RUNE-313 proposed exactly that move; it would have
// widened the window in which every volume reports "not yet mounted".
func TestMountResolverWiredBeforeAgentStart(t *testing.T) {
	nodeSrc := readSource(t, "startup_node.go")
	wiringSrc := readSource(t, "startup_wiring.go")

	if !contains(nodeSrc, "SetMountResolver") {
		t.Error("SetMountResolver must live in startup_node.go, inside subsystem " +
			"registration, which runs BEFORE agent.Start (RUNE-313 D2)")
	}
	if contains(wiringSrc, "SetMountResolver") {
		t.Error("SetMountResolver must NOT be in the post-agent wiring phase: " +
			"registration runs before agent.Start, so moving it here delays the " +
			"swap from notReadyMountResolver and widens the not-yet-mounted window")
	}
	// The publisher is the opposite constraint: it needs node identity, so it
	// belongs in the post-start phase and nowhere earlier.
	if !contains(wiringSrc, "SetEndpointPublisher") {
		t.Error("SetEndpointPublisher belongs in wireNodeEndpoints: it needs " +
			"agent identity, which does not exist until the agent has started")
	}
}
