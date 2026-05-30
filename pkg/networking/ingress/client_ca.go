package ingress

import (
	"crypto/tls"
	"crypto/x509"
	"sync"
)

// ClientCARegistry holds the per-host client-CA pools that drive inbound
// mTLS. The ingress controller writes entries (resolved from each exposed
// service's clientCert.caSecret); the TLS listener reads them at handshake
// time via ConfigFor. Concurrency-safe; reads are the hot path (every
// handshake to an mTLS host), writes happen on reconcile.
//
// A host with an entry requires + verifies a client cert against its pool.
// A host with no entry uses the default config (no client auth) — so
// removing clientCert from a service disables mTLS on the next reconcile.
type ClientCARegistry struct {
	mu     sync.RWMutex
	byHost map[string]*x509.CertPool
}

// NewClientCARegistry returns an empty registry.
func NewClientCARegistry() *ClientCARegistry {
	return &ClientCARegistry{byHost: map[string]*x509.CertPool{}}
}

// Set installs (or replaces) the client-CA pool for host. host is
// normalized to match handshake SNI lookups.
func (r *ClientCARegistry) Set(host string, pool *x509.CertPool) {
	h := normalizeHost(host)
	if h == "" || pool == nil {
		return
	}
	r.mu.Lock()
	r.byHost[h] = pool
	r.mu.Unlock()
}

// Forget removes any client-CA pool for host (disabling mTLS for it).
func (r *ClientCARegistry) Forget(host string) {
	r.mu.Lock()
	delete(r.byHost, normalizeHost(host))
	r.mu.Unlock()
}

// pool returns the client-CA pool for host, ok=false when none.
func (r *ClientCARegistry) pool(host string) (*x509.CertPool, bool) {
	r.mu.RLock()
	p, ok := r.byHost[normalizeHost(host)]
	r.mu.RUnlock()
	return p, ok
}

// HasPool reports whether a client-CA pool is registered for host. The
// listener uses this to fail closed: a route that requires mTLS but has no
// pool loaded (e.g. a misconfigured caSecret) must be refused, not served
// unauthenticated. Nil-safe.
func (r *ClientCARegistry) HasPool(host string) bool {
	if r == nil {
		return false
	}
	_, ok := r.pool(host)
	return ok
}

// ConfigFor implements the per-SNI branch of tls.Config.GetConfigForClient.
// Client-cert verification is negotiated during the handshake — before the
// HTTP Host header exists — so the only routing signal is SNI. When the SNI
// host has a registered CA pool, return a clone of base that requires and
// verifies a client cert against it; otherwise return nil so the TLS stack
// keeps using base (no client auth). base must carry GetCertificate so the
// clone still serves the server cert.
func (r *ClientCARegistry) ConfigFor(hello *tls.ClientHelloInfo, base *tls.Config) *tls.Config {
	if r == nil || hello == nil || base == nil {
		return nil
	}
	pool, ok := r.pool(hello.ServerName)
	if !ok {
		return nil
	}
	c := base.Clone()
	c.ClientCAs = pool
	c.ClientAuth = tls.RequireAndVerifyClientCert
	return c
}
