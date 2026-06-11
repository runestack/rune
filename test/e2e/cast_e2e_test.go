//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/test/e2e/harness"
)

// TestCastService_EndToEnd casts a service file through the real CLI
// against a real runed and verifies the resulting control-plane state
// through the gRPC API. Runs without Docker: --detach returns after
// the release is accepted, and assertions stay on the Service object
// (instances would be LaunchFailed without a runner, which is fine —
// dataplane coverage lives in the Docker-gated tests).
func TestCastService_EndToEnd(t *testing.T) {
	ctx := harness.New(t)

	svcFile := filepath.Join(t.TempDir(), "nginx.yaml")
	if err := os.WriteFile(svcFile, []byte(`
service:
  name: nginx-e2e
  image: nginx:alpine
  scale: 1
`), 0o644); err != nil {
		t.Fatalf("write service file: %v", err)
	}

	res := ctx.CLI.MustRun(t, "cast", svcFile, "--detach", "--release", "nginx-e2e")
	if !res.Contains("nginx-e2e") {
		t.Fatalf("expected cast output to mention the service: %s", res)
	}

	c, cancel := ctx.Ctx()
	defer cancel()
	resp, err := generated.NewServiceServiceClient(ctx.Conn()).GetService(c, &generated.GetServiceRequest{
		Name:      "nginx-e2e",
		Namespace: "default",
	})
	if err != nil {
		t.Fatalf("GetService via API: %v", err)
	}
	svc := resp.GetService()
	if svc.GetName() != "nginx-e2e" {
		t.Fatalf("unexpected service name: %q", svc.GetName())
	}
	if svc.GetImage() != "nginx:alpine" {
		t.Fatalf("unexpected image: %q", svc.GetImage())
	}
	if svc.GetScale() != 1 {
		t.Fatalf("unexpected scale: %d", svc.GetScale())
	}

	// And the user-facing view agrees.
	list := ctx.CLI.MustRun(t, "get", "services")
	if !list.StdoutContains("nginx-e2e") {
		t.Fatalf("expected service in CLI listing: %s", list)
	}
}
