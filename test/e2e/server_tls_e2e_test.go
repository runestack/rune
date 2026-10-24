//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/crypto"
	"github.com/runestack/rune/test/e2e/harness"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// pinned verifies the server by its key fingerprint alone, the way a CLI
// context with a pin will.
func pinned(fp string) credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // replaced by the pin check below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			cert, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			if crypto.SPKIFingerprint(cert) != fp {
				return errors.New("fingerprint mismatch")
			}
			return nil
		},
	})
}

// health reports whether the server answered: Unauthenticated counts, since
// it proves a working transport to the RPC layer.
func health(t *testing.T, addr string, creds credentials.TransportCredentials) error {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = generated.NewHealthServiceClient(conn).GetHealth(ctx, &generated.GetHealthRequest{})
	if err == nil || status.Code(err) == codes.Unauthenticated {
		return nil
	}
	return err
}

func TestServerTLS_SelfSignedPinnedAndPlaintextRefused(t *testing.T) {
	srv := harness.StartServer(t, harness.WithTLS())

	fp, err := crypto.CertFileFingerprint(filepath.Join(srv.DataDir, "tls", "server.crt"))
	if err != nil {
		t.Fatalf("read generated certificate: %v", err)
	}
	// Operators pin the value runed logs, so it must be the real one.
	if !srv.LogsContain(fp) {
		t.Fatalf("runed did not log the fingerprint %s", fp)
	}

	if err := health(t, srv.GRPCAddr, pinned(fp)); err != nil {
		t.Fatalf("pinned TLS dial failed: %v", err)
	}
	if err := health(t, srv.GRPCAddr, pinned("sha256:00")); err == nil {
		t.Fatal("a wrong pin was accepted")
	}
	if err := health(t, srv.GRPCAddr, insecure.NewCredentials()); err == nil {
		t.Fatal("plaintext gRPC was served on a TLS listener")
	}
}
