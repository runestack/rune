//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/upgrade"
	"github.com/runestack/rune/test/e2e/harness"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// upgradeTarget is a published release, so the CLI test resolves real
// checksums. Which release does not matter: staging is refused before any
// download.
const upgradeTarget = "v0.0.1-dev.147"

// TestUpgradeServer_RefusedWithoutUnits covers the refusal a harness runed
// can really reach: it never runs under systemd, so on linux staging stops at
// units-missing, and off linux the stager is not wired at all. The no-systemd
// and upgrade-in-progress refusals need a systemd host and are covered by the
// rune-cloud upgrade drill.
func TestUpgradeServer_RefusedWithoutUnits(t *testing.T) {
	if _, err := os.Stat("/etc/systemd/system/" + upgrade.UpgradePathUnit); err == nil {
		t.Skip("this host has the applier units installed; units-missing is unreachable")
	}
	ctx := harness.New(t)
	c, cancel := ctx.Ctx()
	defer cancel()

	_, err := generated.NewAdminServiceClient(ctx.Conn()).UpgradeServer(c, &generated.UpgradeServerRequest{
		Version: upgradeTarget,
		Sha256:  strings.Repeat("0", 64),
	})
	st, _ := status.FromError(err)
	if runtime.GOOS != "linux" {
		if st.Code() != codes.Unimplemented {
			t.Fatalf("off linux UpgradeServer should be Unimplemented, got %v", err)
		}
		return
	}
	if st.Code() != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	if r := upgrade.PreconditionReason(st.Message()); r != upgrade.ReasonUnitsMissing {
		t.Fatalf("want reason %q, got %q (%s)", upgrade.ReasonUnitsMissing, r, st.Message())
	}
	if _, err := os.Stat(upgrade.StagingDir(ctx.Server.DataDir)); err == nil {
		t.Fatal("a refused upgrade left a staging dir behind")
	}

	// --server keeps the CLI from replacing its own binary.
	res := ctx.CLI.Run(t, "upgrade", upgradeTarget, "--server", "-y")
	if res.Succeeded() {
		t.Fatalf("--server upgrade against a host without units should fail:\n%s", res)
	}
	if res.Contains("fetching checksums.txt") {
		t.Skipf("could not reach the release on GitHub:\n%s", res)
	}
	if !res.Contains("upgrade helper isn't installed") || !res.Contains("upgrade-server.sh") {
		t.Fatalf("want the units-missing explanation and the one-liner remedy, got:\n%s", res)
	}
}
