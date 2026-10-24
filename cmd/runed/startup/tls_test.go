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

func TestServerTLSOptions_UnwritableTLSDirErrors(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataDir, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if _, err := serverTLSOptions(config.TLS{Enabled: true}, false, dataDir, log.NewTestLogger()); err == nil {
		t.Fatal("an unwritable data_dir/tls was accepted")
	}
}

// A typo under [server.tls] decodes as TLS off. With require_tls set, the
// result must still be a loopback-only gRPC bind (see grpcBindAddr), never
// plaintext on a public address.
func TestServerTLSOptions_TypoWithRequireTLSStaysLoopback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runefile.toml")
	body := "[server]\ngrpc_address = \":7863\"\nrequire_tls = true\n\n[server.tls]\nenabeld = true\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := serverTLSOptions(cfg.Server.TLS, cfg.Server.RequireTLS, dir, log.NewTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if o := applied(opts); o.EnableTLS || !o.GRPCRequireTLS {
		t.Fatalf("EnableTLS=%v GRPCRequireTLS=%v; want TLS off and require on", o.EnableTLS, o.GRPCRequireTLS)
	}
}
