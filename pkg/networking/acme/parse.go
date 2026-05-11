// Package acme: helpers that touch crypto/x509 + encoding/pem.
// Kept in a separate file so acme.go stays focused on the state
// machine.
package acme

import (
	"crypto/x509"
	"encoding/pem"
	"time"
)

// parseFirstCertNotAfter walks the PEM blocks in p, decoding the
// first CERTIFICATE block and returning its NotAfter. Any other
// block types are skipped. Returns errNoCert if no certificate is
// found.
func parseFirstCertNotAfter(p []byte) (time.Time, error) {
	rest := p
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return time.Time{}, errNoCert
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return time.Time{}, err
		}
		return c.NotAfter, nil
	}
}
