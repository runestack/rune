package acme

import (
	"context"
	"sync"
)

// MemCertStore is an in-memory CertStore. Suitable for tests and as
// a thin wrapper while the SecretRepo-backed store is being built.
type MemCertStore struct {
	mu sync.RWMutex
	m  map[string]memCert
}

type memCert struct {
	cert []byte
	key  []byte
}

// NewMemCertStore returns an empty MemCertStore.
func NewMemCertStore() *MemCertStore {
	return &MemCertStore{m: map[string]memCert{}}
}

// Set stores cert + key for host atomically.
func (s *MemCertStore) Set(_ context.Context, host string, cert, key []byte) error {
	s.mu.Lock()
	s.m[host] = memCert{cert: append([]byte(nil), cert...), key: append([]byte(nil), key...)}
	s.mu.Unlock()
	return nil
}

// Get returns cert + key for host. Missing host returns (nil, nil, nil).
func (s *MemCertStore) Get(_ context.Context, host string) ([]byte, []byte, error) {
	s.mu.RLock()
	v, ok := s.m[host]
	s.mu.RUnlock()
	if !ok {
		return nil, nil, nil
	}
	return append([]byte(nil), v.cert...), append([]byte(nil), v.key...), nil
}

// Delete removes the cert for host.
func (s *MemCertStore) Delete(_ context.Context, host string) error {
	s.mu.Lock()
	delete(s.m, host)
	s.mu.Unlock()
	return nil
}
