//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/runestack/rune/test/e2e/harness"
)

// pinnedCastToken mints a cast-permission service account pinned to one
// namespace.
func pinnedCastToken(t *testing.T, ctx *harness.Context, name, namespace string) *harness.CLI {
	t.Helper()
	out := filepath.Join(t.TempDir(), "token")
	res := ctx.CLI.Run(t, "admin", "service", "create", name, "--namespace", namespace, "--permissions", "cast", "--out-file", out)
	if !res.Succeeded() {
		t.Fatalf("mint pinned token: %s", res)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read minted token: %v", err)
	}
	return ctx.CLIAs(t, string(b))
}

// A token pinned to one namespace must not pull another namespace's secret
// into its own container through an env template.
func TestCrossNamespaceRefs_PinnedTokenCannotRevealAnotherNamespace(t *testing.T) {
	ctx := harness.New(t)

	ctx.CLI.MustRun(t, "cast", writeManifest(t, `
secrets:
  - name: db
    data:
      password: hunter2
`), "--detach", "--release", "prod-secrets", "--namespace", "prod", "--create-namespace")
	ctx.CLI.MustRun(t, "cast", writeManifest(t, `
service:
  name: placeholder
  image: nginx:alpine
  scale: 0
`), "--detach", "--release", "trial-base", "--namespace", "trial", "--create-namespace")

	thief := writeManifest(t, `
service:
  name: thief
  image: nginx:alpine
  scale: 0
  env:
    DB: "{{secret:db.prod.rune/password}}"
`)
	cli := pinnedCastToken(t, ctx, "ci-trial", "trial")
	res := cli.Run(t, "cast", thief, "--detach", "--release", "thief", "--namespace", "trial")
	if res.Succeeded() {
		t.Fatalf("a trial-pinned token cast a service reading prod's secret; expected denial.\n%s", res)
	}
	if !res.Contains("reveal") {
		t.Fatalf("expected the denial to name secrets:reveal, got:\n%s", res)
	}

	// The admin token, which holds reveal everywhere, may cast it.
	ctx.CLI.MustRun(t, "cast", thief, "--detach", "--release", "thief", "--namespace", "trial")
}
