package startup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/api/server"
	"github.com/runestack/rune/pkg/crypto"
	"github.com/runestack/rune/pkg/log"
)

func applied(opts []server.Option) *server.Options {
	o := server.DefaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	return o
}

func TestServerTLSOptions_Disabled(t *testing.T) {
	opts, err := serverTLSOptions(config.TLS{}, true, t.TempDir(), log.NewTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	o := applied(opts)
	if o.EnableTLS || !o.GRPCRequireTLS {
		t.Fatalf("EnableTLS=%v GRPCRequireTLS=%v", o.EnableTLS, o.GRPCRequireTLS)
	}
}

func TestServerTLSOptions_SelfSigned(t *testing.T) {
	dataDir := t.TempDir()
	opts, err := serverTLSOptions(config.TLS{Enabled: true, Hosts: []string{"rune.example"}}, false, dataDir, log.NewTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	o := applied(opts)
	if !o.EnableTLS || o.TLSCertFile != filepath.Join(dataDir, "tls", "server.crt") {
		t.Fatalf("EnableTLS=%v cert=%q", o.EnableTLS, o.TLSCertFile)
	}
	b, err := os.ReadFile(o.TLSCertFile)
	if err != nil || !strings.Contains(string(b), "CERTIFICATE") {
		t.Fatalf("no certificate written: %v", err)
	}
}

func TestServerTLSOptions_OperatorFiles(t *testing.T) {
	sc, err := crypto.LoadOrCreateServerCert(t.TempDir(), []string{"a"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	opts, err := serverTLSOptions(config.TLS{Enabled: true, CertFile: sc.CertFile, KeyFile: sc.KeyFile}, false, t.TempDir(), log.NewTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if o := applied(opts); !o.EnableTLS || o.TLSCertFile != sc.CertFile || o.TLSKeyFile != sc.KeyFile {
		t.Fatalf("operator files not used: %+v", o)
	}
}

// Enabled TLS that cannot be loaded must stop startup, never fall back to
// plaintext.
func TestServerTLSOptions_FailsClosed(t *testing.T) {
	sc, err := crypto.LoadOrCreateServerCert(t.TempDir(), []string{"a"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "nope.pem")
	for name, cfg := range map[string]config.TLS{
		"cert without key": {Enabled: true, CertFile: sc.CertFile},
		"key without cert": {Enabled: true, KeyFile: sc.KeyFile},
		"unreadable cert":  {Enabled: true, CertFile: missing, KeyFile: sc.KeyFile},
		"mismatched pair":  {Enabled: true, CertFile: sc.CertFile, KeyFile: sc.CertFile},
	} {
		if _, err := serverTLSOptions(cfg, false, t.TempDir(), log.NewTestLogger()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
