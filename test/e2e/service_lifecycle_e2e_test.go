//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/test/e2e/harness"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestServiceLifecycle drives create → scale → delete through the real
// CLI and confirms every transition through the gRPC API. Pure
// control-plane: no Docker required.
func TestServiceLifecycle(t *testing.T) {
	ctx := harness.New(t)
	svcClient := generated.NewServiceServiceClient(ctx.Conn())

	getService := func(name string) (*generated.Service, error) {
		c, cancel := ctx.Ctx()
		defer cancel()
		resp, err := svcClient.GetService(c, &generated.GetServiceRequest{Name: name, Namespace: "default"})
		if err != nil {
			return nil, err
		}
		return resp.GetService(), nil
	}

	// Create.
	// imagePull: missing starts from the locally cached image so the lifecycle
	// test (create → scale → delete of the CONTROL plane) doesn't depend on
	// registry connectivity, and a hung pull can't stall the delete teardown.
	svcFile := filepath.Join(t.TempDir(), "web.yaml")
	if err := os.WriteFile(svcFile, []byte(`
service:
  name: web
  image: nginx:alpine
  imagePull: missing
  scale: 1
`), 0o644); err != nil {
		t.Fatalf("write service file: %v", err)
	}
	ctx.CLI.MustRun(t, "cast", svcFile, "--detach", "--release", "web")

	svc, err := getService("web")
	if err != nil {
		t.Fatalf("service not visible after cast: %v", err)
	}
	if svc.GetScale() != 1 {
		t.Fatalf("expected scale 1 after cast, got %d", svc.GetScale())
	}

	// Scale. --detach: without a container runtime instances never
	// reach Ready, and this test only asserts the control-plane spec.
	ctx.CLI.MustRun(t, "scale", "web", "3", "--detach")
	ctx.Eventually(harness.DefaultConvergeTimeout, "service scale to reach 3", func() bool {
		svc, err := getService("web")
		return err == nil && svc.GetScale() == 3
	})

	// Delete.
	ctx.CLI.MustRun(t, "delete", "service", "web", "--force")
	ctx.Eventually(harness.DefaultConvergeTimeout, "service deletion", func() bool {
		_, err := getService("web")
		return status.Code(err) == codes.NotFound
	})
}
