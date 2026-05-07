package types

import "testing"

func TestHasNodeRole(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		role   string
		want   bool
	}{
		{"empty labels", nil, "edge", false},
		{"empty role", map[string]string{LabelNodeRole: "edge"}, "", false},
		{"exact match", map[string]string{LabelNodeRole: "edge"}, "edge", true},
		{"comma list", map[string]string{LabelNodeRole: "edge,storage"}, "storage", true},
		{"with spaces", map[string]string{LabelNodeRole: "edge, storage"}, "storage", true},
		{"prefix not match", map[string]string{LabelNodeRole: "edge-staging"}, "edge", false},
		{"missing key", map[string]string{"other": "edge"}, "edge", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HasNodeRole(c.labels, c.role); got != c.want {
				t.Fatalf("got=%v want=%v", got, c.want)
			}
		})
	}
}

func TestIsEdgeNode(t *testing.T) {
	if !IsEdgeNode(map[string]string{LabelNodeRole: NodeRoleEdge}) {
		t.Fatal("expected edge")
	}
	if IsEdgeNode(map[string]string{LabelNodeRole: "worker"}) {
		t.Fatal("worker is not edge")
	}
}

func TestValidateExpose_Nil(t *testing.T) {
	if err := ValidateExpose(nil, true); err != nil {
		t.Fatal(err)
	}
}

func TestValidateExpose_PathPrefix(t *testing.T) {
	err := ValidateExpose(&ServiceExpose{Port: "80", Path: "api"}, false)
	if err == nil {
		t.Fatal("expected error for path missing /")
	}
}

func TestValidateExpose_ACMEHostRequired(t *testing.T) {
	err := ValidateExpose(&ServiceExpose{Port: "80", TLS: &ExposeServiceTLS{Mode: "acme"}}, false)
	if err == nil {
		t.Fatal("expected error for acme without host")
	}
	if err := ValidateExpose(&ServiceExpose{Port: "80", Host: "h", TLS: &ExposeServiceTLS{Mode: "acme"}}, false); err != nil {
		t.Fatal(err)
	}
}

func TestValidateExpose_ReservedPortOnEdge(t *testing.T) {
	for _, p := range EdgeReservedPorts {
		if err := ValidateExpose(&ServiceExpose{Port: "80", HostPort: p}, true); err == nil {
			t.Fatalf("expected error for reserved port %d on edge", p)
		}
		if err := ValidateExpose(&ServiceExpose{Port: "80", HostPort: p}, false); err != nil {
			t.Fatalf("port %d should be allowed off-edge: %v", p, err)
		}
	}
}

func TestExposeServiceTLS_IsACME(t *testing.T) {
	if (&ExposeServiceTLS{}).IsACME() {
		t.Fatal("empty should not be acme")
	}
	if !(&ExposeServiceTLS{Mode: ExposeTLSModeACME}).IsACME() {
		t.Fatal("mode=acme should be acme")
	}
	if !(&ExposeServiceTLS{Auto: true}).IsACME() {
		t.Fatal("auto should imply acme")
	}
	if (&ExposeServiceTLS{Auto: true, Mode: ExposeTLSModeManual}).IsACME() {
		t.Fatal("explicit manual should override auto")
	}
}
