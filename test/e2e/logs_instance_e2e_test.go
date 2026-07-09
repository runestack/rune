//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/test/e2e/harness"
)

// TestInstanceLogsWithObservability is the #156 regression test: with RuneSight
// observability ENABLED, `rune logs <instance>` (non-follow, by name and by
// UUID) must return output. The bug: the history path queried only
// {service=<target>}, so an instance target matched nothing, and because it
// still claimed the request the CLI never fell back to the live stream —
// silent empty output. Service logs and --follow were unaffected, which is why
// it hid. Requires Docker (real container producing logs).
func TestInstanceLogsWithObservability(t *testing.T) {
	ctx := harness.New(t, harness.WithObservability("embedded"))

	// A service that logs a unique, greppable token every second. imagePull:
	// missing keeps the test off the registry (cached alpine).
	const token = "rune-156-logline"
	svcFile := filepath.Join(t.TempDir(), "logsvc.yaml")
	if err := os.WriteFile(svcFile, []byte(`
service:
  name: logsvc
  image: alpine:latest
  imagePull: missing
  scale: 1
  command: sh
  args: ["-c","i=0; while true; do i=$((i+1)); echo `+token+`-$i; sleep 1; done"]
`), 0o644); err != nil {
		t.Fatalf("write service file: %v", err)
	}
	ctx.CLI.MustRun(t, "cast", svcFile, "--detach", "--release", "logsvc")

	// Resolve the running instance's name + UUID via the API.
	instClient := generated.NewInstanceServiceClient(ctx.Conn())
	var instName, instID string
	ctx.Eventually(harness.DefaultConvergeTimeout, "instance running", func() bool {
		c, cancel := ctx.Ctx()
		defer cancel()
		resp, err := instClient.ListInstances(c, &generated.ListInstancesRequest{
			Namespace:   "default",
			ServiceName: "logsvc",
		})
		if err != nil {
			return false
		}
		for _, in := range resp.GetInstances() {
			if in.GetStatus() == generated.InstanceStatus_INSTANCE_STATUS_RUNNING {
				instName, instID = in.GetName(), in.GetId()
				return true
			}
		}
		return false
	})
	if instName == "" || instID == "" {
		t.Fatal("did not resolve a running instance name/id")
	}

	// Baseline: service logs work (this always did).
	ctx.Eventually(harness.DefaultConvergeTimeout, "service logs non-empty", func() bool {
		return ctx.CLI.MustRun(t, "logs", "logsvc", "-n", "default", "--tail", "5").StdoutContains(token)
	})

	// The regression: instance logs by NAME and by UUID must not be empty.
	// Observability ingestion is async, so poll.
	ctx.Eventually(harness.DefaultConvergeTimeout, "instance logs by name non-empty", func() bool {
		return ctx.CLI.MustRun(t, "logs", instName, "-n", "default", "--tail", "5").StdoutContains(token)
	})
	ctx.Eventually(harness.DefaultConvergeTimeout, "instance logs by uuid non-empty", func() bool {
		out := ctx.CLI.MustRun(t, "logs", instID, "-n", "default", "--tail", "5").Stdout
		return strings.Contains(out, token)
	})
}
