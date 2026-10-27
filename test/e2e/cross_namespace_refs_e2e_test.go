//go:build e2e
// +build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiclient "github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/test/e2e/harness"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mintCastToken mints a cast-permission service account, pinned to
// namespace when it is non-empty, and returns its bearer token.
func mintCastToken(t *testing.T, ctx *harness.Context, name, namespace string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "token")
	args := []string{"admin", "service", "create", name, "--permissions", "cast", "--out-file", out}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	if res := ctx.CLI.Run(t, args...); !res.Succeeded() {
		t.Fatalf("mint token: %s", res)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read minted token: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// pinnedCastToken returns a CLI authenticated as a cast token pinned to
// namespace.
func pinnedCastToken(t *testing.T, ctx *harness.Context, name, namespace string) *harness.CLI {
	t.Helper()
	return ctx.CLIAs(t, mintCastToken(t, ctx, name, namespace))
}

// connAs dials the server as token.
func connAs(t *testing.T, ctx *harness.Context, token string) *grpc.ClientConn {
	t.Helper()
	c, err := apiclient.NewClient(&apiclient.ClientOptions{
		Address: ctx.Server.GRPCAddr, Token: token,
		DialTimeout: 15 * time.Second, CallTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial as token: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.Conn()
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

// VolumeService writes go through reference admission: the builtin cast
// role can create volumes anywhere but holds neither snapshots:reveal nor
// secrets:reveal, so a volume parameter naming another namespace's snapshot
// or secret is refused.
func TestCrossNamespaceRefs_VolumeParametersNeedTargetGrant(t *testing.T) {
	ctx := harness.New(t)
	vols := generated.NewVolumeServiceClient(connAs(t, ctx, mintCastToken(t, ctx, "ci-vol", "")))
	for name, params := range map[string]map[string]string{
		"snapshot restore": {types.RestoreFromSnapshotParam: "prod/nightly"},
		"secret param":     {"token": "secret:db.prod.rune/password"},
	} {
		c, cancel := ctx.Ctx()
		_, err := vols.CreateVolume(c, &generated.CreateVolumeRequest{Volume: &generated.Volume{
			Name: "v-" + strings.ReplaceAll(name, " ", "-"), Namespace: "trial",
			StorageClassName: "local", Size: "1Gi", AccessMode: "ReadWriteOnce", Parameters: params,
		}, EnsureNamespace: true})
		cancel()
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: want PermissionDenied, got %v", name, err)
		}
	}
}

// RestoreVolume stamps the restore parameter itself, so it is admitted too,
// before the snapshot is even looked up.
func TestCrossNamespaceRefs_RestoreFromAnotherNamespaceNeedsReveal(t *testing.T) {
	ctx := harness.New(t)
	c, cancel := ctx.Ctx()
	defer cancel()
	snaps := generated.NewSnapshotServiceClient(connAs(t, ctx, mintCastToken(t, ctx, "ci-restore", "")))
	_, err := snaps.RestoreVolume(c, &generated.RestoreVolumeRequest{
		SnapshotName: "nightly", SnapshotNamespace: "prod",
		TargetVolumeName: "stolen", TargetNamespace: "trial",
	})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "snapshots") {
		t.Fatalf("want a snapshots:reveal denial, got %v", err)
	}
}
