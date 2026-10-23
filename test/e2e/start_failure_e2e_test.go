//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/test/e2e/harness"
)

// TestStartFailureRetriesKeepOneContainer is the #241 regression test. An
// entrypoint missing from the image passes docker create and fails docker
// start, so every create attempt fails at Start. Each retry must remove the
// previous attempt's container before creating its own: one container per
// instance at all times, and a new one per retry.
func TestStartFailureRetriesKeepOneContainer(t *testing.T) {
	harness.RequireDocker(t)
	ctx := harness.New(t)

	svcFile := filepath.Join(t.TempDir(), "startfail.yaml")
	if err := os.WriteFile(svcFile, []byte(`
service:
  name: startfail
  image: nginx:alpine
  imagePull: missing
  scale: 1
  command: /rune-241-no-such-entrypoint
`), 0o644); err != nil {
		t.Fatalf("write service file: %v", err)
	}
	ctx.CLI.MustRun(t, "cast", svcFile, "--detach", "--release", "startfail")

	instClient := generated.NewInstanceServiceClient(ctx.Conn())
	var instID string
	ctx.Eventually(harness.DefaultConvergeTimeout, "instance recorded as failed", func() bool {
		c, cancel := ctx.Ctx()
		defer cancel()
		resp, err := instClient.ListInstances(c, &generated.ListInstancesRequest{
			Namespace:   "default",
			ServiceName: "startfail",
		})
		if err != nil || len(resp.GetInstances()) != 1 {
			return false
		}
		in := resp.GetInstances()[0]
		instID = in.GetId()
		return in.GetStatus() == generated.InstanceStatus_INSTANCE_STATUS_FAILED
	})

	containers := func() []string {
		out, err := exec.Command("docker", "ps", "-a", "-q", "--no-trunc",
			"--filter", "label=rune.instance.id="+instID).Output()
		if err != nil {
			t.Fatalf("docker ps: %v", err)
		}
		return strings.Fields(string(out))
	}

	// Waits for the next retry: exactly one container, and not the one the
	// previous attempt created. Fails as soon as two exist.
	var current string
	nextAttempt := func(what string, timeout time.Duration) {
		t.Helper()
		ctx.Eventually(timeout, what, func() bool {
			ids := containers()
			if len(ids) > 1 {
				t.Fatalf("%s: %d containers for instance %s, want at most 1: %v", what, len(ids), instID, ids)
			}
			if len(ids) == 1 && ids[0] != current {
				current = ids[0]
				return true
			}
			return false
		})
	}

	// Backoff is 30s then 1m, checked on a 30s reconcile tick.
	nextAttempt("first attempt's container", harness.DefaultConvergeTimeout)
	nextAttempt("first retry replaces the container", 2*time.Minute)
	nextAttempt("second retry replaces the container", 3*time.Minute)
}
