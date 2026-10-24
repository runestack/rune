package crypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadOrCreateServerCert_CreatesUsablePair(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	now := time.Now()
	sc, err := LoadOrCreateServerCert(dir, []string{"node1", "10.0.0.5", "node1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !sc.NewKey || !strings.HasPrefix(sc.Fingerprint, "sha256:") {
		t.Fatalf("unexpected result %+v", sc)
	}
	pair, err := tls.LoadX509KeyPair(sc.CertFile, sc.KeyFile)
	if err != nil {
		t.Fatalf("pair does not load: %v", err)
	}
	cert := pair.Leaf
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Fatalf("want a P-256 key, got %T", cert.PublicKey)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "node1" {
		t.Errorf("DNS SANs = %v", cert.DNSNames)
	}
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.0.0.5" {
		t.Errorf("IP SANs = %v", cert.IPAddresses)
	}
	if err := cert.VerifyHostname("10.0.0.5"); err != nil {
		t.Errorf("IP SAN does not verify: %v", err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, sc.KeyFile: 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}

func TestLoadOrCreateServerCert_ReusesUntilSomethingChanges(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	first, err := LoadOrCreateServerCert(dir, []string{"a"}, now)
	if err != nil {
		t.Fatal(err)
	}
	certBytes := func() string {
		b, err := os.ReadFile(first.CertFile)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	orig := certBytes()

	again, err := LoadOrCreateServerCert(dir, []string{"a"}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if again.NewKey || certBytes() != orig {
		t.Fatal("an unchanged host set must reuse key and certificate")
	}

	// A new address or the approach of expiry re-issues the certificate,
	// but under the same key, so the pinned fingerprint holds.
	steps := []struct {
		name string
		at   time.Time
	}{
		{"host added", now},
		{"near expiry", now.Add(340 * 24 * time.Hour)},
	}
	for _, step := range steps {
		name := step.name
		before := certBytes()
		sc, err := LoadOrCreateServerCert(dir, []string{"a", "10.1.1.1"}, step.at)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if certBytes() == before {
			t.Errorf("%s: certificate was not re-issued", name)
		}
		if sc.NewKey || sc.Fingerprint != first.Fingerprint {
			t.Errorf("%s: fingerprint changed %s -> %s", name, first.Fingerprint, sc.Fingerprint)
		}
	}
}

// A corrupt key must not be replaced quietly: that would invalidate every
// client's pin with no signal.
func TestLoadOrCreateServerCert_RefusesCorruptKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateServerCert(dir, []string{"a"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, serverKeyFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateServerCert(dir, []string{"a"}, time.Now()); err == nil {
		t.Fatal("corrupt key accepted")
	}
}

func TestCertFileFingerprintMatchesGenerated(t *testing.T) {
	sc, err := LoadOrCreateServerCert(t.TempDir(), []string{"a"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fp, err := CertFileFingerprint(sc.CertFile)
	if err != nil || fp != sc.Fingerprint {
		t.Fatalf("fingerprint %q, %v; want %q", fp, err, sc.Fingerprint)
	}
}
