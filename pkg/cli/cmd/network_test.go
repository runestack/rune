package cmd

import (
	"testing"

	"github.com/runestack/rune/pkg/networking/policy"
	"github.com/runestack/rune/pkg/types"
)

// A process-runtime service's egress rules are never applied, because
// the agent cannot resolve a process instance to a service identity.
// `rune get netpolicy` must not report default-deny egress for one.
func TestEgressUnenforcedReason(t *testing.T) {
	pol := &types.ServiceNetworkPolicy{
		Egress: []types.EgressRule{
			{To: []types.NetworkPolicyPeer{{Service: "db", Namespace: "infra"}}},
		},
	}

	proc := &types.Service{ID: "w", Name: "w", Namespace: "prod", Runtime: types.RuntimeTypeProcess, NetworkPolicy: pol}
	out := policy.Compile(proc).Explain()
	if got := egressUnenforcedReason(proc, out); got == "" {
		t.Fatal("process runtime with egress rules must be reported as unenforced")
	}

	ctr := &types.Service{ID: "w", Name: "w", Namespace: "prod", Runtime: types.RuntimeTypeContainer, NetworkPolicy: pol}
	if got := egressUnenforcedReason(ctr, policy.Compile(ctr).Explain()); got != "" {
		t.Fatalf("container runtime is enforced; got %q", got)
	}

	// No egress rules at all: nothing to warn about, whatever the runtime.
	bare := &types.Service{ID: "w", Name: "w", Namespace: "prod", Runtime: types.RuntimeTypeProcess}
	if got := egressUnenforcedReason(bare, policy.Compile(bare).Explain()); got != "" {
		t.Fatalf("no egress rules means no warning; got %q", got)
	}
}
