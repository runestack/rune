package ingress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"

	"github.com/runestack/rune/pkg/networking/acme"
)

// CertLoader fronts an acme.CertStore with an in-process LRU-style
// cache so TLS handshakes do not hit Badger on every connection.
//
// Reload(ctx, host) refreshes the cached entry; the orchestrator
// calls Reload after a successful Set on the underlying CertStore.
// Hot-reload is therefore push-based (orchestrator -> loader) with
// a pull-based fallback if the cache misses.
type CertLoader struct {
	store acme.CertStore

	mu    sync.RWMutex
	cache map[string]*tls.Certificate
}

// NewCertLoader builds a loader backed by store.
func NewCertLoader(store acme.CertStore) *CertLoader {
	return &CertLoader{store: store, cache: map[string]*tls.Certificate{}}
}

// GetCertificate satisfies tls.Config.GetCertificate. It is called
// by the TLS stack on every handshake.
func (l *CertLoader) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := normalizeHost(hello.ServerName)
	if host == "" {
		return nil, errors.New("ingress: TLS handshake missing SNI")
	}
	l.mu.RLock()
	c, ok := l.cache[host]
	l.mu.RUnlock()
	if ok {
		return c, nil
	}
	if err := l.Reload(hello.Context(), host); err != nil {
		return nil, err
	}
	l.mu.RLock()
	c, ok = l.cache[host]
	l.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("ingress: no cert for host %q", host)
	}
	return c, nil
}

// Reload fetches the latest cert for host from the store and updates
// the cache. Errors include "no cert exists yet" (ok = nil cert,
// nil err — Reload then leaves the cache empty so handshakes fail
// with a clear error rather than silently serving a stale cert).
func (l *CertLoader) Reload(ctx context.Context, host string) error {
	host = normalizeHost(host)
	if host == "" {
		return errors.New("ingress: Reload: empty host")
	}
	cert, key, err := l.store.Get(ctx, host)
	if err != nil {
		return fmt.Errorf("ingress: load cert for %q: %w", host, err)
	}
	if cert == nil || key == nil {
		l.mu.Lock()
		delete(l.cache, host)
		l.mu.Unlock()
		return nil
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return fmt.Errorf("ingress: parse cert for %q: %w", host, err)
	}
	l.mu.Lock()
	l.cache[host] = &pair
	l.mu.Unlock()
	return nil
}

// Forget removes the cached cert for host.
func (l *CertLoader) Forget(host string) {
	l.mu.Lock()
	delete(l.cache, normalizeHost(host))
	l.mu.Unlock()
}
