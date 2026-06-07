package forwarder

import (
	"testing"

	"github.com/runestack/rune/pkg/types"
)

// The forwarder must carry the instance's denormalized service labels onto every
// log record so they become queryable LogQL stream dimensions (e.g. {app="web"}),
// alongside the intrinsic dimensions (namespace/service/instance/node).
func TestRecordFromInstanceLine_CarriesServiceLabels(t *testing.T) {
	s := &Subsystem{cfg: Config{NodeID: "node-1"}}
	inst := &types.Instance{
		Namespace:   "default",
		ServiceName: "web",
		ID:          "b3c1f2a0-0000-0000-0000-000000000001",
		Name:        "web-0",
		Labels:      map[string]string{"app": "web", "tier": "frontend"},
	}

	rec := s.recordFromInstanceLine(inst, "hello world")

	if rec.Labels["app"] != "web" || rec.Labels["tier"] != "frontend" {
		t.Fatalf("service labels not propagated: %+v", rec.Labels)
	}
	// Instance is stamped with the friendly Name, not the opaque ID/UUID.
	if rec.Namespace != "default" || rec.Service != "web" || rec.Instance != "web-0" || rec.Node != "node-1" {
		t.Errorf("intrinsic dims wrong: ns=%q svc=%q inst=%q node=%q", rec.Namespace, rec.Service, rec.Instance, rec.Node)
	}

	// StreamLabels unions custom labels with the fixed dims; fixed dims win on
	// key collisions so user labels can't shadow intrinsic identity.
	sl := rec.StreamLabels()
	if sl["app"] != "web" {
		t.Errorf("custom label missing from StreamLabels: %+v", sl)
	}
	if sl["service"] != "web" || sl["namespace"] != "default" {
		t.Errorf("intrinsic dims missing from StreamLabels: %+v", sl)
	}
}

// An instance with no labels yields no custom stream labels (nil-safe).
func TestRecordFromInstanceLine_NoLabels(t *testing.T) {
	s := &Subsystem{cfg: Config{NodeID: "node-1"}}
	rec := s.recordFromInstanceLine(&types.Instance{Namespace: "default", ServiceName: "api", ID: "api-0"}, "x")
	if len(rec.Labels) != 0 {
		t.Errorf("want no custom labels, got %+v", rec.Labels)
	}
}
