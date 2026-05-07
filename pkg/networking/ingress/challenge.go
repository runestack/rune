package ingress

import "sync"

// MemChallengeStore is a goroutine-safe in-memory ChallengeStore
// suitable for single-node deployments. Multi-node will replace
// this with a SecretRepo-backed store so every edge node serves
// the same token (RUNE-066b).
type MemChallengeStore struct {
	mu sync.RWMutex
	m  map[string]string
}

// NewMemChallengeStore returns an empty MemChallengeStore.
func NewMemChallengeStore() *MemChallengeStore {
	return &MemChallengeStore{m: map[string]string{}}
}

// Put stores keyAuth under token.
func (s *MemChallengeStore) Put(token, keyAuth string) {
	s.mu.Lock()
	s.m[token] = keyAuth
	s.mu.Unlock()
}

// Delete removes the token. Safe to call when token is not present.
func (s *MemChallengeStore) Delete(token string) {
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

// Get returns the keyAuth for token. ok=false when absent.
func (s *MemChallengeStore) Get(token string) (string, bool) {
	s.mu.RLock()
	v, ok := s.m[token]
	s.mu.RUnlock()
	return v, ok
}
