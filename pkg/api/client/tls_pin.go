package client

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"

	"google.golang.org/grpc/credentials"
)

// pinnedTransportCreds verifies the server by the SPKI fingerprint pin alone,
// skipping hostname and chain verification. The server certificate is
// self-signed and usually IP-addressed, so a CA chain and a hostname do not
// apply; the pin — SHA-256 of the leaf's SubjectPublicKeyInfo, which survives
// a certificate re-issue under the same key — is the trust anchor. pin is
// "sha256:<hex>", as runed logs it and `rune context trust` records it.
func pinnedTransportCreds(pin string) (credentials.TransportCredentials, error) {
	want, err := parsePin(pin)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // verified by the SPKI pin below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("tls pin: server presented no certificate")
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("tls pin: parse server certificate: %w", err)
			}
			if !spkiMatches(leaf, want) {
				return errors.New("tls pin: server certificate fingerprint does not match the pinned value")
			}
			return nil
		},
	}), nil
}

// parsePin decodes a "sha256:<hex>" pin into its 32 raw bytes.
func parsePin(pin string) ([]byte, error) {
	const prefix = "sha256:"
	if len(pin) <= len(prefix) || pin[:len(prefix)] != prefix {
		return nil, fmt.Errorf("tls pin %q must be %q followed by hex", pin, prefix)
	}
	b, err := hex.DecodeString(pin[len(prefix):])
	if err != nil {
		return nil, fmt.Errorf("tls pin %q: %w", pin, err)
	}
	if len(b) != sha256.Size {
		return nil, fmt.Errorf("tls pin %q: want %d bytes, got %d", pin, sha256.Size, len(b))
	}
	return b, nil
}

func spkiMatches(cert *x509.Certificate, want []byte) bool {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return string(sum[:]) == string(want)
}
