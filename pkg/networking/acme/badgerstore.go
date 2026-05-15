package acme

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

// CertNamespace is the namespace used by BadgerCertStore. Built-in
// (seeded by SeedBuiltinNamespaces); operators see one Secret per
// host here when they `rune get secrets -n system`.
const CertNamespace = "system"

// certNamePrefix distinguishes ACME-issued certs from any other
// operator-managed Secrets in the system namespace.
const certNamePrefix = "acme-cert-"

// certDataCert / certDataKey are the data keys inside the Secret.
// Matches the well-known names a `tls.mode: manual` SecretName is
// expected to use, keeping the two stores shape-compatible.
const (
	certDataCert = "tls.crt"
	certDataKey  = "tls.key"
)

// BadgerCertStore persists ACME-issued certs through the existing
// SecretRepo (BadgerDB + the runed KEK encryption pipeline). One
// Secret per host under the `system` namespace.
//
// Wired in cmd/runed in place of MemCertStore so that runed restarts
// reuse the cert that's already on disk instead of re-issuing against
// the ACME provider — the production outage the Propeller team hit
// when our in-memory MemCertStore meant every restart counted as a
// fresh LE issuance against the per-identifier-set 168h rate limit.
type BadgerCertStore struct {
	repo *repos.SecretRepo
}

// NewBadgerCertStore returns a CertStore backed by the given store.
// The caller is responsible for ensuring the SecretRepo's KEK is
// already configured (same KEK as the rest of the runed store).
//
// SecretRepo's defaults reject zero-length key names, which would
// otherwise drop our tls.crt / tls.key writes on the floor. We
// inject our own limits at the cert-store layer so the operator's
// runefile [secret.limits] section can't accidentally shrink the
// envelope below what an ACME cert + key need to be persisted.
func NewBadgerCertStore(st store.Store, opts ...repos.SecretOption) *BadgerCertStore {
	merged := append([]repos.SecretOption{
		repos.WithSecretLimits(store.Limits{
			MaxKeyNameLength: 64,      // "tls.crt" / "tls.key" are 7
			MaxObjectBytes:   1 << 20, // 1 MiB — well above a PEM bundle + key
		}),
	}, opts...)
	return &BadgerCertStore{repo: repos.NewSecretRepo(st, merged...)}
}

// Set atomically writes cert + key for host. Idempotent — repeated
// Set calls for the same host overwrite the previous value (the
// SecretRepo bumps the internal version each time).
func (s *BadgerCertStore) Set(ctx context.Context, host string, cert, key []byte) error {
	if host == "" {
		return errors.New("acme: Set: empty host")
	}
	name := certNameFor(host)
	data := map[string]string{
		certDataCert: string(cert),
		certDataKey:  string(key),
	}

	if existing, err := s.repo.Get(ctx, CertNamespace, name); err == nil && existing != nil {
		existing.Data = data
		return s.repo.Update(ctx, CertNamespace, name, existing)
	} else if err != nil && !store.IsNotFoundError(err) {
		return fmt.Errorf("acme: load existing cert for %q: %w", host, err)
	}

	return s.repo.Create(ctx, &types.Secret{
		Name:      name,
		Namespace: CertNamespace,
		Type:      "static",
		Data:      data,
	})
}

// Get returns (nil, nil, nil) when no cert exists for host. Callers
// (the orchestrator's check-before-issue gate) rely on the (nil, nil)
// sentinel rather than an error to keep the happy path clean.
func (s *BadgerCertStore) Get(ctx context.Context, host string) ([]byte, []byte, error) {
	if host == "" {
		return nil, nil, errors.New("acme: Get: empty host")
	}
	sec, err := s.repo.Get(ctx, CertNamespace, certNameFor(host))
	if err != nil {
		if store.IsNotFoundError(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("acme: load cert for %q: %w", host, err)
	}
	cert := []byte(sec.Data[certDataCert])
	key := []byte(sec.Data[certDataKey])
	if len(cert) == 0 || len(key) == 0 {
		return nil, nil, nil
	}
	return cert, key, nil
}

// Delete removes the persisted cert for host. Idempotent on a
// missing host.
func (s *BadgerCertStore) Delete(ctx context.Context, host string) error {
	if host == "" {
		return errors.New("acme: Delete: empty host")
	}
	err := s.repo.Delete(ctx, CertNamespace, certNameFor(host))
	if err != nil && !store.IsNotFoundError(err) {
		return err
	}
	return nil
}

// certNameFor returns the Secret name used to persist host's cert.
// SHA-256 hex of the lowercased host, prefixed for namespacing and
// debuggability when grepping the badger store. Output is always
// DNS-1123 valid: 11 ASCII characters of prefix plus 32 lowercase
// hex characters = 43, comfortably under the 63-char DNS label cap.
//
// Hashing trades reversibility (which Get/Set don't need) for
// guaranteed DNS-1123 compliance regardless of how exotic the host
// looks (wildcards, punycode, trailing dots, …). The host is
// preserved verbatim as a label on the Secret for operator debugging.
func certNameFor(host string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(host)))
	return certNamePrefix + hex.EncodeToString(sum[:16])
}

// NeedsRenewal reports whether certPEM should be renewed now,
// applied as (notAfter <= now + renewBefore). A parse failure is
// treated as "yes, renew" so a malformed persisted cert doesn't
// pin the orchestrator forever.
func NeedsRenewal(certPEM []byte, now time.Time, renewBefore time.Duration) bool {
	expiry, err := certNotAfter(certPEM)
	if err != nil {
		return true
	}
	return !expiry.After(now.Add(renewBefore))
}
