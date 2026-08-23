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

// castToken mints a service account with exactly the built-in `cast`
// permission set — the deliberately-narrow CI credential — and returns
// a CLI authenticated as it.
func castToken(t *testing.T, ctx *harness.Context, name string) *harness.CLI {
	t.Helper()
	out := filepath.Join(t.TempDir(), "token")
	res := ctx.CLI.Run(t, "admin", "service", "create", name, "--permissions", "cast", "--out-file", out)
	if !res.Succeeded() {
		t.Fatalf("mint cast token: %s", res)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read minted token: %v", err)
	}
	return ctx.CLIAs(t, string(b))
}

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "svc.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return p
}

// TestPrivilegedGate_CastPath asserts the services:privileged gate is
// evaluated on the cast path. `rune cast` travels ReleaseService/Cast,
// whose applier calls orchestrator CreateService in-process, so a gate
// wired to ServiceService method names does not cover it.
func TestPrivilegedGate_CastPath(t *testing.T) {
	ctx := harness.New(t)
	cli := castToken(t, ctx, "ci-privileged")

	svcFile := writeManifest(t, `
service:
  name: priv-cast
  image: nginx:alpine
  scale: 1
  securityContext:
    privileged: true
`)

	res := cli.Run(t, "cast", svcFile, "--detach", "--release", "priv-cast")
	if res.Succeeded() {
		t.Fatalf("cast with privileged:true succeeded for a `cast`-scoped token; expected denial.\n%s", res)
	}
	if !res.Contains("privileged") {
		t.Fatalf("expected the denial to name the services:privileged verb, got:\n%s", res)
	}

	// And nothing landed in the store.
	c, cancel := ctx.Ctx()
	defer cancel()
	if _, err := generated.NewServiceServiceClient(ctx.Conn()).GetService(c, &generated.GetServiceRequest{
		Name: "priv-cast", Namespace: "default",
	}); err == nil {
		t.Fatal("privileged service was persisted despite the denial")
	}
}

// TestPrivilegedGate_SeccompUnconfinedCastPath covers the second knob
// carried by the same gate.
func TestPrivilegedGate_SeccompUnconfinedCastPath(t *testing.T) {
	ctx := harness.New(t)
	cli := castToken(t, ctx, "ci-seccomp")

	svcFile := writeManifest(t, `
service:
  name: seccomp-cast
  image: nginx:alpine
  scale: 1
  securityContext:
    seccompProfile:
      type: unconfined
`)

	res := cli.Run(t, "cast", svcFile, "--detach", "--release", "seccomp-cast")
	if res.Succeeded() {
		t.Fatalf("cast with seccomp=unconfined succeeded for a `cast`-scoped token; expected denial.\n%s", res)
	}
}

// TestPrivilegedGate_InitStepCastPath covers a gated securityContext on
// an init step rather than the main container.
func TestPrivilegedGate_InitStepCastPath(t *testing.T) {
	ctx := harness.New(t)
	cli := castToken(t, ctx, "ci-initstep")

	svcFile := writeManifest(t, `
service:
  name: initstep-cast
  image: nginx:alpine
  scale: 1
  initSteps:
    - name: format
      image: busybox
      command: /bin/true
      runIf:
        type: always
      securityContext:
        privileged: true
`)

	res := cli.Run(t, "cast", svcFile, "--detach", "--release", "initstep-cast")
	if res.Succeeded() {
		t.Fatalf("cast with a privileged init step succeeded for a `cast`-scoped token; expected denial.\n%s", res)
	}
}

// TestPrivilegedGate_CastPathAdminAllowed proves the gate is a gate and
// not a blanket ban: the admin token still casts the same manifest.
func TestPrivilegedGate_CastPathAdminAllowed(t *testing.T) {
	ctx := harness.New(t)

	svcFile := writeManifest(t, `
service:
  name: priv-admin
  image: nginx:alpine
  scale: 1
  securityContext:
    privileged: true
`)

	ctx.CLI.MustRun(t, "cast", svcFile, "--detach", "--release", "priv-admin")

	c, cancel := ctx.Ctx()
	defer cancel()
	resp, err := generated.NewServiceServiceClient(ctx.Conn()).GetService(c, &generated.GetServiceRequest{
		Name: "priv-admin", Namespace: "default",
	})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if !resp.GetService().GetSecurityContext().GetPrivileged() {
		t.Fatal("admin cast did not preserve privileged:true")
	}
}

// TestPrivilegedGate_NonPrivilegedCastStillWorks guards against the fix
// over-blocking ordinary CI deploys.
func TestPrivilegedGate_NonPrivilegedCastStillWorks(t *testing.T) {
	ctx := harness.New(t)
	cli := castToken(t, ctx, "ci-plain")

	svcFile := writeManifest(t, `
service:
  name: plain-cast
  image: nginx:alpine
  scale: 1
`)

	cli.MustRun(t, "cast", svcFile, "--detach", "--release", "plain-cast")

	c, cancel := ctx.Ctx()
	defer cancel()
	if _, err := generated.NewServiceServiceClient(ctx.Conn()).GetService(c, &generated.GetServiceRequest{
		Name: "plain-cast", Namespace: "default",
	}); err != nil {
		t.Fatalf("plain cast should still work: %v", err)
	}
}
