package docker

import (
	"testing"

	"github.com/runestack/rune/pkg/types"
)

// List() reconstructs instances from container labels. It used to stamp a
// literal "local" while the same machine's volumes and log streams were
// keyed by the agent's real node ID.
func TestDockerRunner_NodeID(t *testing.T) {
	r := &DockerRunner{}
	if got := r.NodeID(); got != types.LocalNodeIDFallback {
		t.Fatalf("unwired NodeID() = %q, want %q", got, types.LocalNodeIDFallback)
	}
	r.SetNodeID("node-8f6a12cd")
	if got := r.NodeID(); got != "node-8f6a12cd" {
		t.Fatalf("NodeID() = %q, want %q", got, "node-8f6a12cd")
	}
	// An empty set must not blank the identity out.
	r.SetNodeID("")
	if got := r.NodeID(); got != types.LocalNodeIDFallback {
		t.Fatalf("NodeID() after empty set = %q, want %q", got, types.LocalNodeIDFallback)
	}
}
