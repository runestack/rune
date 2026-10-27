//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiclient "github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/crypto"
	"github.com/runestack/rune/test/e2e/harness"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The SDK client dials a TLS runed when given UseTLS + the server's SPKI pin;
// a wrong pin and a plaintext dial both fail. This drives the client's own
// pinnedTransportCreds path (not a hand-rolled tls.Config).
func TestClientTLS_PinnedDialWorksWrongPinAndPlaintextFail(t *testing.T) {
	srv := harness.StartServer(t, harness.WithTLS())
	pin, err := crypto.CertFileFingerprint(filepath.Join(srv.DataDir, "tls", "server.crt"))
	if err != nil {
		t.Fatalf("read server fingerprint: %v", err)
	}

	health := func(o *apiclient.ClientOptions) error {
		o.Address = srv.GRPCAddr
		o.DialTimeout = 10 * time.Second
		o.CallTimeout = 10 * time.Second
		c, err := apiclient.NewClient(o)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, herr := generated.NewHealthServiceClient(c.Conn()).GetHealth(ctx, &generated.GetHealthRequest{})
		if herr == nil || status.Code(herr) == codes.Unauthenticated {
			return nil
		}
		return herr
	}

	if err := health(&apiclient.ClientOptions{UseTLS: true, TLSPinSHA256: pin}); err != nil {
		t.Fatalf("pinned dial failed: %v", err)
	}
	if err := health(&apiclient.ClientOptions{UseTLS: true, TLSPinSHA256: "sha256:" + strings.Repeat("ab", 32)}); err == nil {
		t.Fatal("a wrong pin connected")
	}
	if err := health(&apiclient.ClientOptions{UseTLS: false}); err == nil {
		t.Fatal("a plaintext dial connected to a TLS server")
	}
}
