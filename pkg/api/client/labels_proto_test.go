package client

import (
	"testing"

	"github.com/runestack/rune/pkg/types"
)

// Labels must survive the types <-> proto round trip for both services and
// instances, so the API/dashboard can display resource labels (regression: the
// proto Instance message had no labels field and ServiceToProto dropped them).
func TestProtoRoundTrip_ServiceLabels(t *testing.T) {
	svc := &types.Service{
		Name:      "web",
		Namespace: "default",
		Labels:    map[string]string{"app": "web", "tier": "frontend"},
	}
	got, err := ProtoToService(ServiceToProto(svc))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if got.Labels["app"] != "web" || got.Labels["tier"] != "frontend" {
		t.Errorf("service labels lost in proto round trip: %+v", got.Labels)
	}
}

func TestProtoRoundTrip_InstanceLabels(t *testing.T) {
	inst := &types.Instance{
		Name:      "web-0",
		Namespace: "default",
		Labels:    map[string]string{"app": "web", "tier": "frontend"},
	}
	back := embeddedInstanceFromProto(embeddedInstanceToProto(inst))
	if back == nil {
		t.Fatal("nil instance after round trip")
	}
	if back.Labels["app"] != "web" || back.Labels["tier"] != "frontend" {
		t.Errorf("instance labels lost in proto round trip: %+v", back.Labels)
	}
}
