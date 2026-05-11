package policy

import (
	"net"
	"testing"

	"github.com/runestack/rune/pkg/types"
)

func mkSvc(id, ns string, pol *types.ServiceNetworkPolicy) *types.Service {
	return &types.Service{ID: id, Namespace: ns, NetworkPolicy: pol}
}

func TestCompile_NilOrEmptyReturnsNil(t *testing.T) {
	if Compile(nil) != nil {
		t.Fatal("expected nil for nil service")
	}
	if Compile(mkSvc("a", "ns", nil)) != nil {
		t.Fatal("expected nil for nil policy")
	}
	if Compile(mkSvc("a", "ns", &types.ServiceNetworkPolicy{})) != nil {
		t.Fatal("expected nil for empty policy")
	}
}

func TestEvaluateIngress_NoPolicyAllows(t *testing.T) {
	r := (*Compiled)(nil).EvaluateIngress(PeerInfo{IP: net.ParseIP("10.0.0.1")}, 80, "tcp")
	if r.Decision != DecisionNoPolicy {
		t.Fatalf("want NoPolicy, got %v", r.Decision)
	}
}

func TestEvaluateIngress_DefaultDeny(t *testing.T) {
	pol := &types.ServiceNetworkPolicy{
		Ingress: []types.IngressRule{
			{From: []types.NetworkPolicyPeer{{Service: "web", Namespace: "prod"}}},
		},
	}
	c := Compile(mkSvc("api", "prod", pol))
	if c == nil {
		t.Fatal("nil compiled")
	}
	if !c.HasIngressRules() {
		t.Fatal("default-deny not active")
	}
	// peer with no identity → deny
	r := c.EvaluateIngress(PeerInfo{IP: net.ParseIP("10.0.0.5")}, 80, "tcp")
	if r.Decision != DecisionDeny {
		t.Fatalf("want Deny, got %v", r.Decision)
	}
}

func TestEvaluateIngress_AllowByService_SameNodeOnly(t *testing.T) {
	pol := &types.ServiceNetworkPolicy{
		Ingress: []types.IngressRule{
			{From: []types.NetworkPolicyPeer{{Service: "web", Namespace: "prod"}}, Ports: []string{"80/tcp"}},
		},
	}
	c := Compile(mkSvc("api", "prod", pol))

	id := types.InstanceIdentity{InstanceID: "i1", Service: "web", Namespace: "prod"}
	// same-node peer → allow
	r := c.EvaluateIngress(PeerInfo{IP: net.ParseIP("172.17.0.5"), Identity: &id, SameNode: true}, 80, "tcp")
	if r.Decision != DecisionAllow {
		t.Fatalf("want Allow same-node, got %v reason=%s", r.Decision, r.Reason)
	}
	// cross-node, no identity → deny with cross-node reason
	r = c.EvaluateIngress(PeerInfo{IP: net.ParseIP("10.0.0.9")}, 80, "tcp")
	if r.Decision != DecisionDeny {
		t.Fatalf("want Deny cross-node, got %v", r.Decision)
	}
	if r.Reason != ReasonDeniedCrossNodeIdent {
		t.Fatalf("want cross-node reason, got %s", r.Reason)
	}
}

func TestEvaluateIngress_AllowByCIDR_AnyNode(t *testing.T) {
	pol := &types.ServiceNetworkPolicy{
		Ingress: []types.IngressRule{
			{From: []types.NetworkPolicyPeer{{CIDR: "10.0.0.0/24"}}},
		},
	}
	c := Compile(mkSvc("api", "prod", pol))
	r := c.EvaluateIngress(PeerInfo{IP: net.ParseIP("10.0.0.42")}, 443, "tcp")
	if r.Decision != DecisionAllow || r.Reason != ReasonAllowedByCIDR {
		t.Fatalf("want Allow/CIDR, got %v/%s", r.Decision, r.Reason)
	}
	r = c.EvaluateIngress(PeerInfo{IP: net.ParseIP("11.0.0.1")}, 443, "tcp")
	if r.Decision != DecisionDeny {
		t.Fatalf("want Deny outside CIDR, got %v", r.Decision)
	}
}

func TestEvaluateIngress_PortMismatch(t *testing.T) {
	pol := &types.ServiceNetworkPolicy{
		Ingress: []types.IngressRule{
			{From: []types.NetworkPolicyPeer{{CIDR: "0.0.0.0/0"}}, Ports: []string{"80"}},
		},
	}
	c := Compile(mkSvc("api", "prod", pol))
	r := c.EvaluateIngress(PeerInfo{IP: net.ParseIP("1.2.3.4")}, 81, "tcp")
	if r.Decision != DecisionDeny || r.Reason != ReasonDeniedPort {
		t.Fatalf("want Deny/port, got %v/%s", r.Decision, r.Reason)
	}
}

func TestEvaluateIngress_PortProtoMatch(t *testing.T) {
	pol := &types.ServiceNetworkPolicy{
		Ingress: []types.IngressRule{
			{From: []types.NetworkPolicyPeer{{CIDR: "0.0.0.0/0"}}, Ports: []string{"53/udp"}},
		},
	}
	c := Compile(mkSvc("dns", "prod", pol))
	if r := c.EvaluateIngress(PeerInfo{IP: net.ParseIP("1.2.3.4")}, 53, "udp"); r.Decision != DecisionAllow {
		t.Fatalf("want Allow udp, got %v", r.Decision)
	}
	if r := c.EvaluateIngress(PeerInfo{IP: net.ParseIP("1.2.3.4")}, 53, "tcp"); r.Decision != DecisionDeny {
		t.Fatalf("want Deny tcp, got %v", r.Decision)
	}
}

func TestEvaluateEgress_AllowByServiceNamespace(t *testing.T) {
	pol := &types.ServiceNetworkPolicy{
		Egress: []types.EgressRule{
			{To: []types.NetworkPolicyPeer{{Service: "db", Namespace: "infra"}}, Ports: []string{"5432/tcp"}},
		},
	}
	c := Compile(mkSvc("api", "prod", pol))
	if !c.HasEgressRules() {
		t.Fatal("egress default-deny not active")
	}
	r := c.EvaluateEgress(EgressTarget{Service: "db", Namespace: "infra", Port: 5432}, "tcp")
	if r.Decision != DecisionAllow {
		t.Fatalf("want Allow, got %v", r.Decision)
	}
	r = c.EvaluateEgress(EgressTarget{Service: "db", Namespace: "other", Port: 5432}, "tcp")
	if r.Decision != DecisionDeny {
		t.Fatalf("want Deny different ns, got %v", r.Decision)
	}
	r = c.EvaluateEgress(EgressTarget{Service: "cache", Namespace: "infra", Port: 5432}, "tcp")
	if r.Decision != DecisionDeny {
		t.Fatalf("want Deny different svc, got %v", r.Decision)
	}
}

func TestExplain_DeterministicOutput(t *testing.T) {
	pol := &types.ServiceNetworkPolicy{
		Ingress: []types.IngressRule{
			{From: []types.NetworkPolicyPeer{{CIDR: "10.0.0.0/24"}, {Service: "web", Namespace: "prod"}}, Ports: []string{"80"}},
		},
	}
	c := Compile(mkSvc("api", "prod", pol))
	out := c.Explain()
	if !out.DefaultDenyIngress {
		t.Fatal("default-deny ingress should be reported")
	}
	if len(out.Ingress) != 1 || len(out.Ingress[0].Peers) != 2 || out.Ingress[0].Ports[0] != "80" {
		t.Fatalf("unexpected explain shape: %+v", out)
	}
}

func TestValidate_RejectsBadCIDR(t *testing.T) {
	pol := &types.ServiceNetworkPolicy{
		Ingress: []types.IngressRule{
			{From: []types.NetworkPolicyPeer{{CIDR: "not-a-cidr"}}},
		},
	}
	if err := Validate(pol); err == nil {
		t.Fatal("want error for bad CIDR")
	}
}

func TestParsePort(t *testing.T) {
	cases := map[string]struct {
		ok    bool
		port  int
		proto string
	}{
		"80":     {true, 80, ""},
		"80/tcp": {true, 80, "tcp"},
		"53/udp": {true, 53, "udp"},
		"99999":  {false, 0, ""},
		"foo":    {false, 0, ""},
		"80/x":   {false, 0, ""},
		"":       {false, 0, ""},
	}
	for in, want := range cases {
		got, ok := parsePort(in)
		if ok != want.ok || (ok && (got.port != want.port || got.proto != want.proto)) {
			t.Errorf("parsePort(%q) = %+v ok=%v, want %+v ok=%v", in, got, ok, want, want.ok)
		}
	}
}

func TestLocalInstancesTable_ApplyLookupRemove(t *testing.T) {
	tab := NewLocalInstancesTable("nodeA")
	tab.Apply(types.LocalInstances{
		NodeID: "nodeA",
		Instances: map[string]types.InstanceIdentity{
			"172.17.0.5": {InstanceID: "i1", Service: "web", Namespace: "prod"},
		},
	})
	tab.Apply(types.LocalInstances{
		NodeID: "nodeB",
		Instances: map[string]types.InstanceIdentity{
			"10.0.1.7": {InstanceID: "i2", Service: "api", Namespace: "prod"},
		},
	})

	id, ok := tab.Lookup(net.ParseIP("172.17.0.5"))
	if !ok || id.Service != "web" {
		t.Fatalf("lookup nodeA failed: %v / %v", id, ok)
	}
	if !tab.SameNode(net.ParseIP("172.17.0.5")) {
		t.Fatal("SameNode should be true for nodeA peer")
	}
	if tab.SameNode(net.ParseIP("10.0.1.7")) {
		t.Fatal("SameNode should be false for nodeB peer")
	}

	pi := tab.PeerInfoFor(net.ParseIP("172.17.0.5"))
	if pi.Identity == nil || !pi.SameNode {
		t.Fatalf("PeerInfoFor wrong: %+v", pi)
	}

	tab.Remove("nodeA")
	if _, ok := tab.Lookup(net.ParseIP("172.17.0.5")); ok {
		t.Fatal("Remove did not drop nodeA entry")
	}
	if tab.Size() != 1 {
		t.Fatalf("want 1 entry after remove, got %d", tab.Size())
	}
}

func TestEvaluateIngress_NamespaceMatchSameNode(t *testing.T) {
	pol := &types.ServiceNetworkPolicy{
		Ingress: []types.IngressRule{
			{From: []types.NetworkPolicyPeer{{Namespace: "prod"}}},
		},
	}
	c := Compile(mkSvc("api", "prod", pol))
	id := types.InstanceIdentity{Service: "x", Namespace: "prod"}
	r := c.EvaluateIngress(PeerInfo{IP: net.ParseIP("172.17.0.5"), Identity: &id, SameNode: true}, 80, "tcp")
	if r.Decision != DecisionAllow || r.Reason != ReasonAllowedByNamespace {
		t.Fatalf("want Allow/namespace, got %v/%s", r.Decision, r.Reason)
	}
}
