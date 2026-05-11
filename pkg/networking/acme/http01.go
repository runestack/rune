package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"time"

	xacme "golang.org/x/crypto/acme"
)

// HTTP01Issuer is a production Issuer that talks to an ACME v2
// server (Let's Encrypt by default) and completes the HTTP-01
// challenge by publishing the keyAuth into the configured
// ChallengeStore. The edge ingress listener serves the token.
//
// The issuer registers an account on first use and caches the
// account key in memory; persistent account-key storage is a
// follow-up (RUNE-066b).
type HTTP01Issuer struct {
	// Directory is the ACME directory URL. Defaults to Let's Encrypt
	// production. Use xacme.LetsEncryptURL or a Pebble URL in tests.
	Directory string

	// Email is the ACME account contact email. Optional but
	// recommended for renewal notifications.
	Email string

	// Challenges is where the issuer stores HTTP-01 keyAuth values.
	// The ingress listener reads from the same store.
	Challenges ChallengeStore

	// AcceptTOS is called on registration; default accepts.
	AcceptTOS func(tosURL string) bool

	mu      sync.Mutex
	client  *xacme.Client
	account *xacme.Account
}

// Issue obtains a certificate for host using the HTTP-01 challenge.
// Returns PEM-encoded cert chain and PEM PKCS#8 private key.
func (i *HTTP01Issuer) Issue(ctx context.Context, host string) ([]byte, []byte, error) {
	if i.Challenges == nil {
		return nil, nil, errors.New("acme: HTTP01Issuer.Challenges is nil")
	}
	if host == "" {
		return nil, nil, errors.New("acme: empty host")
	}
	cli, err := i.ensureClient(ctx)
	if err != nil {
		return nil, nil, err
	}

	order, err := cli.AuthorizeOrder(ctx, []xacme.AuthzID{{Type: "dns", Value: host}})
	if err != nil {
		return nil, nil, fmt.Errorf("authorize order: %w", err)
	}
	defer cleanupOrder(ctx, cli, order)

	for _, authzURL := range order.AuthzURLs {
		if err := i.solveAuthz(ctx, cli, authzURL); err != nil {
			return nil, nil, err
		}
	}

	// Generate cert key (ECDSA P-256).
	keyPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: host},
		DNSNames: []string{host},
	}, keyPriv)
	if err != nil {
		return nil, nil, fmt.Errorf("create csr: %w", err)
	}
	derChain, _, err := cli.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, nil, fmt.Errorf("finalize order: %w", err)
	}
	if len(derChain) == 0 {
		return nil, nil, errors.New("acme: empty cert chain returned")
	}
	certPEM := encodeCertChain(derChain)
	keyPEM, err := encodeECKey(keyPriv)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

func (i *HTTP01Issuer) solveAuthz(ctx context.Context, cli *xacme.Client, authzURL string) error {
	authz, err := cli.GetAuthorization(ctx, authzURL)
	if err != nil {
		return fmt.Errorf("get authz: %w", err)
	}
	if authz.Status == xacme.StatusValid {
		return nil
	}
	var chal *xacme.Challenge
	for _, c := range authz.Challenges {
		if c.Type == "http-01" {
			chal = c
			break
		}
	}
	if chal == nil {
		return fmt.Errorf("acme: no http-01 challenge for %s", authz.Identifier.Value)
	}
	keyAuth, err := cli.HTTP01ChallengeResponse(chal.Token)
	if err != nil {
		return fmt.Errorf("http01 response: %w", err)
	}
	i.Challenges.Put(chal.Token, keyAuth)
	defer i.Challenges.Delete(chal.Token)

	if _, err := cli.Accept(ctx, chal); err != nil {
		return fmt.Errorf("accept challenge: %w", err)
	}
	if _, err := cli.WaitAuthorization(ctx, authzURL); err != nil {
		return fmt.Errorf("wait authz: %w", err)
	}
	return nil
}

func cleanupOrder(ctx context.Context, cli *xacme.Client, order *xacme.Order) {
	// Best-effort. ACME servers garbage-collect stale orders, so a
	// failure here is not fatal.
	_ = ctx
	_ = cli
	_ = order
}

func (i *HTTP01Issuer) ensureClient(ctx context.Context) (*xacme.Client, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.client != nil {
		return i.client, nil
	}
	dir := i.Directory
	if dir == "" {
		dir = xacme.LetsEncryptURL
	}
	accKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate account key: %w", err)
	}
	cli := &xacme.Client{
		Key:          accKey,
		DirectoryURL: dir,
	}
	tos := i.AcceptTOS
	if tos == nil {
		tos = xacme.AcceptTOS
	}
	acct := &xacme.Account{}
	if i.Email != "" {
		acct.Contact = []string{"mailto:" + i.Email}
	}
	regCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	registered, err := cli.Register(regCtx, acct, tos)
	if err != nil {
		return nil, fmt.Errorf("register account: %w", err)
	}
	i.account = registered
	i.client = cli
	return cli, nil
}

func encodeCertChain(derChain [][]byte) []byte {
	var out []byte
	for _, der := range derChain {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	return out
}

func encodeECKey(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
