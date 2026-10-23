//go:build e2e
// +build e2e

package e2e

import (
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/test/e2e/harness"
)

// A cast-scoped token holds no delete, so it must not delete a service by
// adopting it into a release it controls and then casting that release
// without it. The release path checks each change against the verb its
// direct RPC needs, so the prune is denied before anything changes.
func TestReleaseVerbs_CastTokenCannotPruneAdopted(t *testing.T) {
	ctx := harness.New(t)

	victim := writeManifest(t, `
service:
  name: victim
  image: nginx:alpine
  scale: 0
`)
	ctx.CLI.MustRun(t, "cast", victim, "--detach", "--release", "owner")

	cli := castToken(t, ctx, "ci-prune")
	both := writeManifest(t, `
services:
  - name: web
    image: nginx:alpine
    scale: 0
  - name: victim
    image: nginx:alpine
    scale: 0
`)
	// Adoption is an update, which the cast role holds.
	if res := cli.Run(t, "cast", both, "--detach", "--adopt", "--release", "takeover"); !res.Succeeded() {
		t.Fatalf("cast-scoped adopt: %s", res)
	}

	webOnly := writeManifest(t, `
service:
  name: web
  image: nginx:alpine
  scale: 0
`)
	// Not --detach: a detached cast refuses any prune on its own, which
	// would hide whether authorization ran.
	res := cli.Run(t, "cast", webOnly, "--yes", "--release", "takeover")
	if res.Succeeded() {
		t.Fatalf("cast-scoped token pruned an adopted service; expected denial.\n%s", res)
	}
	if !res.Contains("delete") {
		t.Fatalf("expected the denial to name the delete verb, got:\n%s", res)
	}

	c, cancel := ctx.Ctx()
	defer cancel()
	if _, err := generated.NewServiceServiceClient(ctx.Conn()).GetService(c, &generated.GetServiceRequest{
		Name: "victim", Namespace: "default",
	}); err != nil {
		t.Fatalf("victim was deleted despite the denial: %v", err)
	}
}

// Volumes ride the release path like every other kind, so the cast role
// carries the volume verbs a plain stateful cast needs.
func TestReleaseVerbs_CastTokenCastsVolume(t *testing.T) {
	ctx := harness.New(t)
	cli := castToken(t, ctx, "ci-volume")
	f := writeManifest(t, `
volumes:
  - name: data
    storageClassName: local
    size: 1Gi
`)
	if res := cli.Run(t, "cast", f, "--detach", "--release", "vol"); !res.Succeeded() {
		t.Fatalf("cast-scoped volume cast denied: %s", res)
	}
}
