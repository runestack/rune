//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/test/e2e/harness"
)

// TestDataplane_InstanceRuns is the Tier-2 test: with a Docker daemon
// present, a cast service must converge to an actually-running
// container. Skipped automatically when Docker is unavailable.
func TestDataplane_InstanceRuns(t *testing.T) {
	harness.RequireDocker(t)
	ctx := harness.New(t)

	svcFile := filepath.Join(t.TempDir(), "web.yaml")
	if err := os.WriteFile(svcFile, []byte(`
service:
  name: web-dp
  image: nginx:alpine
  scale: 1
`), 0o644); err != nil {
		t.Fatalf("write service file: %v", err)
	}
	ctx.CLI.MustRun(t, "cast", svcFile, "--detach", "--release", "web-dp")

	instClient := generated.NewInstanceServiceClient(ctx.Conn())
	// Image pull dominates this window on cold runners.
	ctx.Eventually(3*time.Minute, "an instance of web-dp to be Running", func() bool {
		c, cancel := ctx.Ctx()
		defer cancel()
		resp, err := instClient.ListInstances(c, &generated.ListInstancesRequest{Namespace: "default"})
		if err != nil {
			return false
		}
		for _, inst := range resp.GetInstances() {
			if inst.GetServiceName() == "web-dp" && inst.GetStatus() == generated.InstanceStatus_INSTANCE_STATUS_RUNNING {
				return true
			}
		}
		return false
	})

	// Tear down so the container does not outlive the test.
	ctx.CLI.MustRun(t, "delete", "service", "web-dp", "--force")
	ctx.Eventually(harness.DefaultConvergeTimeout, "instances to be gone", func() bool {
		c, cancel := ctx.Ctx()
		defer cancel()
		resp, err := instClient.ListInstances(c, &generated.ListInstancesRequest{Namespace: "default"})
		if err != nil {
			return false
		}
		for _, inst := range resp.GetInstances() {
			if inst.GetServiceName() == "web-dp" && inst.GetStatus() == generated.InstanceStatus_INSTANCE_STATUS_RUNNING {
				return false
			}
		}
		return true
	})
}
