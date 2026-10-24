package types

import (
	"strings"
	"testing"
)

// Liveness successThreshold > 1 used to be accepted and ignored; it is
// refused rather than silently changing when existing services restart.
func TestHealthCheckValidate_LivenessSuccessThreshold(t *testing.T) {
	probe := func(st int) *Probe { return &Probe{Type: "tcp", Port: 80, SuccessThreshold: st} }

	if err := (&HealthCheck{Liveness: probe(1)}).Validate(); err != nil {
		t.Fatalf("successThreshold 1 must be accepted: %v", err)
	}
	err := (&HealthCheck{Liveness: probe(3)}).Validate()
	if err == nil || !strings.Contains(err.Error(), "successThreshold must be 1") {
		t.Fatalf("liveness successThreshold 3 must be refused, got %v", err)
	}
	if err := (&HealthCheck{Readiness: probe(3)}).Validate(); err != nil {
		t.Fatalf("readiness honours successThreshold: %v", err)
	}
}
