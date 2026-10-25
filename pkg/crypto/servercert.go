package crypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	serverCertFile = "server.crt"
	serverKeyFile  = "server.key"

	serverCertValidity = 365 * 24 * time.Hour
	// serverCertRenewBefore re-issues a certificate this close to expiry.
	// Clients pin the key, not the certificate, so re-issuing breaks no pin.
	serverCertRenewBefore = 30 * 24 * time.Hour
)

// ServerCert is a TLS key pair on disk and the fingerprint clients pin.
type ServerCert struct {
	CertFile    string
	KeyFile     string
	Fingerprint string
	// NewKey reports that this call generated the key.
	NewKey bool
	// ReplacedKey reports a new key where a certificate already existed:
	// the key file was missing, so every client's pin is now wrong.
	ReplacedKey bool
	// Tightened lists paths whose permissions were wider than 0700/0600
	// and have been narrowed; the caller should say so.
	Tightened []string
}

// LoadOrCreateServerCert returns the self-signed server certificate in dir,
// creating it on first use. The key is kept across calls; the certificate is
// re-issued under the same key when it nears expiry or when hosts no longer
// match its SANs, so a pinned fingerprint survives both.
func LoadOrCreateServerCert(dir string, hosts []string, now time.Time) (*ServerCert, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create tls dir: %w", err)
	}
	certPath := filepath.Join(dir, serverCertFile)
	keyPath := filepath.Join(dir, serverKeyFile)
	dns, ips := splitHosts(hosts)

	tightened, err := tightenModes(map[string]os.FileMode{dir: 0o700, keyPath: 0o600})
	if err != nil {
		return nil, err
	}
	_, certErr := os.Stat(certPath)
	certExisted := certErr == nil

	key, err := readECKey(keyPath)
	newKey := false
	if errors.Is(err, os.ErrNotExist) {
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, fmt.Errorf("generate tls key: %w", err)
		}
		if err := writePEM(keyPath, "EC PRIVATE KEY", mustMarshalEC(key), 0o600); err != nil {
			return nil, err
		}
		newKey = true
	} else if err != nil {
		// A corrupt key is not silently replaced: that would change the
		// fingerprint every client has pinned.
		return nil, err
	}

	cert, err := readCert(certPath)
	if newKey || err != nil || !certCurrent(cert, &key.PublicKey, dns, ips, now) {
		der, err := issueServerCert(key, dns, ips, now)
		if err != nil {
			return nil, err
		}
		if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
			return nil, err
		}
		if cert, err = x509.ParseCertificate(der); err != nil {
			return nil, err
		}
	}
	return &ServerCert{
		CertFile:    certPath,
		KeyFile:     keyPath,
		Fingerprint: SPKIFingerprint(cert),
		NewKey:      newKey,
		ReplacedKey: newKey && certExisted,
		Tightened:   tightened,
	}, nil
}

// SPKIFingerprint is the SHA-256 of the certificate's public key, as
// "sha256:<hex>". It is what clients pin: it stays fixed when the
// certificate is re-issued under the same key.
func SPKIFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CertFileFingerprint returns SPKIFingerprint of the first certificate in a
// PEM file.
func CertFileFingerprint(path string) (string, error) {
	cert, err := readCert(path)
	if err != nil {
		return "", err
	}
	return SPKIFingerprint(cert), nil
}

func splitHosts(hosts []string) (dns []string, ips []net.IP) {
	seen := map[string]bool{}
	for _, h := range hosts {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, h)
		}
	}
	return dns, ips
}

func certCurrent(cert *x509.Certificate, pub *ecdsa.PublicKey, dns []string, ips []net.IP, now time.Time) bool {
	if cert == nil || !pub.Equal(cert.PublicKey) {
		return false
	}
	if now.Before(cert.NotBefore) || cert.NotAfter.Sub(now) < serverCertRenewBefore {
		return false
	}
	return sameStrings(cert.DNSNames, dns) && sameStrings(ipStrings(cert.IPAddresses), ipStrings(ips))
}

func issueServerCert(key *ecdsa.PrivateKey, dns []string, ips []net.IP, now time.Time) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "runed"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(serverCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// A leaf, never a CA: a client that trusts it via --ca-file must
		// not accept certificates it signs.
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	return x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
}

func readECKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("tls key %s: not PEM", path)
	}
	key, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("tls key %s: %w", path, err)
	}
	return key, nil
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("tls cert %s: no PEM certificate", path)
	}
	return x509.ParseCertificate(blk.Bytes)
}

func mustMarshalEC(key *ecdsa.PrivateKey) []byte {
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic(err) // only fails for curves Go cannot encode; P-256 always encodes
	}
	return b
}

// writePEM writes atomically so a crash never leaves a truncated key that
// would then be refused as corrupt.
func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := pem.Encode(tmp, &pem.Block{Type: typ, Bytes: der}); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// tightenModes applies tighten to each path and returns the ones it narrowed.
func tightenModes(modes map[string]os.FileMode) ([]string, error) {
	var narrowed []string
	for path, want := range modes {
		ok, err := tighten(path, want)
		if err != nil {
			return nil, err
		}
		if ok {
			narrowed = append(narrowed, path)
		}
	}
	slices.Sort(narrowed)
	return narrowed, nil
}

// tighten narrows path to want when its mode is wider, and reports whether
// it did. A missing path is left alone. It refuses a symlink: Stat/Chmod
// would follow it and narrow (or expose) the target, so a symlinked key or
// tls dir is an error, not something to silently chmod through.
func tighten(path string, want os.FileMode) (bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%s is a symlink; refusing to operate on TLS material through a link", path)
	}
	if fi.Mode().Perm()&^want == 0 {
		return false, nil
	}
	if err := os.Chmod(path, want); err != nil {
		return false, fmt.Errorf("narrow %s to %o: %w", path, want, err)
	}
	return true, nil
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}

func sameStrings(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
