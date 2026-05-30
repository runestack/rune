package types

import (
	"strings"
	"testing"
)

func TestValidateExpose_AllowCIDRs(t *testing.T) {
	// Valid list passes.
	e := &ServiceExpose{
		Port: "http", Host: "api.example.com",
		AllowCIDRs: []string{"173.245.48.0/20", "2606:4700::/32"},
	}
	if err := ValidateExpose(e, false); err != nil {
		t.Fatalf("valid CIDRs rejected: %v", err)
	}

	// Empty list is fine (means no restriction).
	if err := ValidateExpose(&ServiceExpose{Port: "http", AllowCIDRs: nil}, false); err != nil {
		t.Fatalf("empty allowCidrs rejected: %v", err)
	}

	// A bad entry is rejected with an index-bearing message.
	bad := &ServiceExpose{
		Port: "http", Host: "api.example.com",
		AllowCIDRs: []string{"173.245.48.0/20", "not-a-cidr"},
	}
	err := ValidateExpose(bad, false)
	if err == nil {
		t.Fatal("expected invalid CIDR to be rejected")
	}
	if !strings.Contains(err.Error(), "allowCidrs[1]") || !strings.Contains(err.Error(), "not-a-cidr") {
		t.Errorf("error should name the offending index/value: %v", err)
	}

	// A bare IP without a mask is not a CIDR — must be rejected (forces
	// operators to be explicit, e.g. /32).
	if err := ValidateExpose(&ServiceExpose{Port: "http", AllowCIDRs: []string{"173.245.48.5"}}, false); err == nil {
		t.Error("bare IP without mask should be rejected as a CIDR")
	}
}

func TestValidateExpose_ClientCert(t *testing.T) {
	host := "api.example.com"

	// Valid: caSecret set, mode require.
	if err := ValidateExpose(&ServiceExpose{Port: "http", Host: host,
		ClientCert: &ExposeClientCert{CASecret: "common/ca", Mode: "require"}}, false); err != nil {
		t.Fatalf("valid clientCert rejected: %v", err)
	}
	// Valid: empty mode defaults to require.
	if err := ValidateExpose(&ServiceExpose{Port: "http", Host: host,
		ClientCert: &ExposeClientCert{CASecret: "common/ca"}}, false); err != nil {
		t.Fatalf("empty-mode clientCert rejected: %v", err)
	}
	// Missing caSecret.
	if err := ValidateExpose(&ServiceExpose{Port: "http", Host: host,
		ClientCert: &ExposeClientCert{Mode: "require"}}, false); err == nil {
		t.Error("clientCert without caSecret should be rejected")
	}
	// Unsupported mode (e.g. optional, cut from v1).
	if err := ValidateExpose(&ServiceExpose{Port: "http", Host: host,
		ClientCert: &ExposeClientCert{CASecret: "common/ca", Mode: "optional"}}, false); err == nil {
		t.Error("clientCert mode 'optional' should be rejected in v1")
	}
	// clientCert requires a host (handshake routes by SNI).
	if err := ValidateExpose(&ServiceExpose{Port: "http",
		ClientCert: &ExposeClientCert{CASecret: "common/ca"}}, false); err == nil {
		t.Error("clientCert without host should be rejected")
	}
}
