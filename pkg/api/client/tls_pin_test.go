package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"testing"
	"time"
)

func TestParsePin(t *testing.T) {
	good := "sha256:" + hex.EncodeToString(make([]byte, 32))
	if _, err := parsePin(good); err != nil {
		t.Fatalf("valid pin rejected: %v", err)
	}
	for _, bad := range []string{"", "sha256:", "nope:" + hex.EncodeToString(make([]byte, 32)), "sha256:zz", "sha256:" + hex.EncodeToString(make([]byte, 16))} {
		if _, err := parsePin(bad); err == nil {
			t.Errorf("bad pin %q accepted", bad)
		}
	}
}

func TestSPKIMatches(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	if !spkiMatches(cert, sum[:]) {
		t.Fatal("matching SPKI did not match")
	}
	wrong := sum
	wrong[0] ^= 0xff
	if spkiMatches(cert, wrong[:]) {
		t.Fatal("wrong SPKI matched")
	}
}

// A ClientOptions with a pin builds without error; a malformed pin fails at
// construction rather than silently falling back to no verification.
func TestNewClient_BadPinFailsClosed(t *testing.T) {
	_, err := NewClient(&ClientOptions{Address: "127.0.0.1:1", UseTLS: true, TLSPinSHA256: "sha256:zz", DialTimeout: time.Second})
	if err == nil {
		t.Fatal("a malformed pin was accepted")
	}
}
