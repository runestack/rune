package client

import (
	"reflect"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
)

// TestServiceProto_NetworkPolicyRoundTrip locks in that the embedded service
// network policy survives the gRPC converter both ways — otherwise a service
// created through the proto API silently loses its ingress/egress rules.
func TestServiceProto_NetworkPolicyRoundTrip(t *testing.T) {
	svc := &types.Service{
		Name: "api", Namespace: "app", Image: "nginx", Scale: 1,
		NetworkPolicy: &types.ServiceNetworkPolicy{
			Ingress: []types.IngressRule{{
				From: []types.NetworkPolicyPeer{
					{Service: "web", Namespace: "app"},
					{ServiceSelector: map[string]string{"tier": "frontend"}},
					{CIDR: "10.0.0.0/8"},
				},
				Ports: []string{"3000/tcp"},
			}},
			Egress: []types.EgressRule{{
				To:    []types.NetworkPolicyPeer{{Service: "postgres"}},
				Ports: []string{"5432/tcp"},
			}},
		},
	}

	got, err := ProtoToService(ServiceToProto(svc))
	if err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if !reflect.DeepEqual(svc.NetworkPolicy, got.NetworkPolicy) {
		t.Fatalf("network policy not preserved\n want %+v\n  got %+v", svc.NetworkPolicy, got.NetworkPolicy)
	}
}

// TestServiceProto_IngressCertToProto checks the read-only ingress cert status
// reaches the wire (the dashboard/CLI surface it). It's status-only, so only
// the to-proto direction is populated.
func TestServiceProto_IngressCertToProto(t *testing.T) {
	exp := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	svc := &types.Service{
		Name: "site", Namespace: "app", Image: "nginx", Scale: 1,
		IngressCert: &types.IngressCertStatus{
			Host:      "example.com",
			State:     types.IngressCertIssued,
			ExpiresAt: &exp,
		},
	}
	p := ServiceToProto(svc)
	if p.IngressCert == nil {
		t.Fatal("ingress cert dropped by converter")
	}
	if p.IngressCert.Host != "example.com" || p.IngressCert.State != "Issued" {
		t.Fatalf("unexpected cert: host=%q state=%q", p.IngressCert.Host, p.IngressCert.State)
	}
	if p.IngressCert.ExpiresAt != exp.Format(time.RFC3339) {
		t.Fatalf("expires_at not RFC3339: %q", p.IngressCert.ExpiresAt)
	}
}
