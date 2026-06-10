//go:build e2e
// +build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/test/e2e/harness"
)

// TestHarness_ServerSurfaces proves one harness boot exposes all three
// access paths — CLI, gRPC SDK, and the HTTP listener — against a real
// runed.
func TestHarness_ServerSurfaces(t *testing.T) {
	ctx := harness.New(t)

	t.Run("grpc health reports components", func(t *testing.T) {
		c, cancel := ctx.Ctx()
		defer cancel()
		resp, err := generated.NewHealthServiceClient(ctx.Conn()).GetHealth(c, &generated.GetHealthRequest{})
		if err != nil {
			t.Fatalf("GetHealth: %v", err)
		}
		if len(resp.Components) == 0 {
			t.Fatal("expected at least one health component")
		}
	})

	t.Run("grpc reports server version", func(t *testing.T) {
		c, cancel := ctx.Ctx()
		defer cancel()
		resp, err := generated.NewHealthServiceClient(ctx.Conn()).GetServerVersion(c, &generated.GetServerVersionRequest{})
		if err != nil {
			t.Fatalf("GetServerVersion: %v", err)
		}
		if resp.GetVersion() == "" {
			t.Fatal("expected non-empty server version")
		}
	})

	t.Run("cli lists builtin namespaces", func(t *testing.T) {
		res := ctx.CLI.MustRun(t, "get", "namespaces")
		for _, ns := range []string{"default", "system"} {
			if !res.StdoutContains(ns) {
				t.Fatalf("expected namespace %q in output: %s", ns, res)
			}
		}
	})

	t.Run("http listener serves", func(t *testing.T) {
		resp, err := ctx.HTTPGet("/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()
		// Root redirects to the dashboard when the UI is enabled; any
		// non-5xx answer proves the HTTP stack is wired.
		if resp.StatusCode >= http.StatusInternalServerError {
			t.Fatalf("unexpected status %d from HTTP listener", resp.StatusCode)
		}
	})

	t.Run("server log captured", func(t *testing.T) {
		if !ctx.LogsContain("Starting Rune Server") {
			t.Fatal("expected startup banner in captured server log")
		}
	})
}
