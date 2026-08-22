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
	"github.com/runestack/rune/test/e2e/harness"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestInstanceRead_DoesNotLeakResolvedSecrets is the end-to-end guard for the
// privilege-boundary bypass where any `readonly` token could read every
// resolved secret value in the cluster: the orchestrator writes the instance's
// fully-resolved environment onto the instance record, and the instance read
// path served it verbatim to anyone holding instances:get/list — a strictly
// weaker grant than the secrets:reveal verb that gates the same plaintext.
//
// Needs Docker: the environment is only resolved once an instance actually
// launches.
func TestInstanceRead_DoesNotLeakResolvedSecrets(t *testing.T) {
	harness.RequireDocker(t)
	ctx := harness.New(t)

	const (
		interpolated = "interp-plaintext-must-not-leak"
		fromEnvFrom  = "envfrom-plaintext-must-not-leak"
		mounted      = "mounted-plaintext-must-not-leak"
	)

	specFile := filepath.Join(t.TempDir(), "leaky.yaml")
	if err := os.WriteFile(specFile, []byte(`
secrets:
  - name: leak-app-secrets
    data:
      DB_PASSWORD: `+interpolated+`
      API_KEY: `+mounted+`
  - name: leak-bulk-secrets
    data:
      BULK_TOKEN: `+fromEnvFrom+`

services:
  - name: leaky
    image: nginx:alpine
    scale: 1
    secretMounts:
      - name: creds
        mountPath: /etc/creds
        secretName: leak-app-secrets
    envFrom:
      - secret: leak-bulk-secrets
    env:
      DB_PASSWORD: "{{secret:leak-app-secrets/DB_PASSWORD}}"
`), 0o644); err != nil {
		t.Fatalf("write spec file: %v", err)
	}
	ctx.CLI.MustRun(t, "cast", specFile, "--detach", "--release", "leaky")

	adminInst := generated.NewInstanceServiceClient(ctx.Conn())
	// Image pull dominates the first convergence on a cold runner.
	ctx.Eventually(3*time.Minute, "leaky to be Running", func() bool {
		return runningInstanceCount(t, ctx, adminInst, "leaky") == 1
	})

	// A readonly token: instances get/list/watch, no secrets:reveal.
	tokenFile := filepath.Join(t.TempDir(), "readonly.token")
	ctx.CLI.MustRun(t, "admin", "token", "create",
		"--subject-id", "auditor", "--subject-type", "user",
		"--name", "auditor-ro", "--policy", "readonly",
		"--out-file", tokenFile)
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("read readonly token: %v", err)
	}
	roClient, err := apiclient.NewClient(&apiclient.ClientOptions{
		Address:     ctx.Server.GRPCAddr,
		Token:       strings.TrimSpace(string(raw)),
		DialTimeout: 15 * time.Second,
		CallTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial readonly client: %v", err)
	}
	defer func() { _ = roClient.Close() }()

	// Control: the same token must not be able to reveal the secret. If this
	// ever starts passing, the comparison below stops meaning anything.
	c, cancel := ctx.Ctx()
	defer cancel()
	_, err = generated.NewSecretServiceClient(roClient.Conn()).
		RevealSecret(c, &generated.RevealSecretRequest{Namespace: "default", Name: "leak-app-secrets"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("readonly RevealSecret must be PermissionDenied (not merely an error); "+
			"the RBAC premise of this test is broken: %v", err)
	}

	resp, err := generated.NewInstanceServiceClient(roClient.Conn()).
		ListInstances(c, &generated.ListInstancesRequest{Namespace: "default"})
	if err != nil {
		t.Fatalf("readonly ListInstances: %v", err)
	}
	wire, err := protojson.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	for _, sentinel := range []string{interpolated, fromEnvFrom, mounted} {
		if strings.Contains(string(wire), sentinel) {
			t.Fatalf("readonly ListInstances leaked resolved secret %q; response: %s", sentinel, wire)
		}
	}
}
