//go:build e2e
// +build e2e

package e2e

import (
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/test/e2e/harness"
)

// TestObservability_EmbeddedBackend boots runed with RuneSight
// persistence on the embedded backend (no external services) and
// checks the ObserveService handshake reflects it — the dashboard and
// `rune logs` decide their feature set from this. Also pins the
// default: a plain harness server must report observability disabled.
func TestObservability_EmbeddedBackend(t *testing.T) {
	ctx := harness.New(t, harness.WithObservability("embedded"))

	c, cancel := ctx.Ctx()
	defer cancel()
	caps, err := generated.NewObserveServiceClient(ctx.Conn()).GetCapabilities(c, &generated.CapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if !caps.GetEnabled() {
		t.Fatal("expected observability enabled with embedded backend")
	}
	if caps.GetBackend() != "embedded" {
		t.Fatalf("expected backend %q, got %q", "embedded", caps.GetBackend())
	}
	if !ctx.LogsContain("Native observability enabled") {
		t.Fatal("expected observability init in server log")
	}
}

func TestObservability_DisabledByDefault(t *testing.T) {
	ctx := harness.New(t)

	c, cancel := ctx.Ctx()
	defer cancel()
	caps, err := generated.NewObserveServiceClient(ctx.Conn()).GetCapabilities(c, &generated.CapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if caps.GetEnabled() {
		t.Fatal("expected observability disabled on a default harness server")
	}
}
