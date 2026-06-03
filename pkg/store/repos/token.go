package repos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

type TokenRepo struct{ st store.Store }

func NewTokenRepo(st store.Store) *TokenRepo { return &TokenRepo{st: st} }

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// HashSecret exposes the canonical secret hashing used for token storage so
// callers outside this package (e.g. the refresh manager) can match a presented
// secret against stored hashes without re-implementing it.
func HashSecret(secret string) string { return hashSecret(secret) }

// newSecret mints a fresh prefixed token secret.
func newSecret() string {
	return TokenSecretPrefix + uuid.NewString() + "." + uuid.NewString()
}

// TokenSecretPrefix is the human-visible prefix on every Rune token
// secret. It exists so the secrets are easy to spot in the wild
// (logs, screenshots, terraform output, leaked configs) and so that
// secret scanners (gitleaks, GitHub secret scanning, etc.) can match
// on a stable, distinctive marker. The prefix is mandatory: tokens
// presented without it are rejected at validation time.
const TokenSecretPrefix = "rune_"

// Issue creates a new long-lived (legacy) token with a freshly generated secret.
// Returns the plaintext secret once. Legacy tokens are full bearers and are not
// subject to RUNE-201 refresh; service accounts and break-glass use this path.
func (r *TokenRepo) Issue(ctx context.Context, name, subjectID, subjectType string, desc string, ttl time.Duration) (*types.Token, string, error) {
	return r.issue(ctx, name, subjectID, subjectType, desc, ttl, types.TokenKindStatic)
}

// IssueAccess mints a short-lived access token (RUNE-201). Accepted as a request
// bearer; expires after ttl.
func (r *TokenRepo) IssueAccess(ctx context.Context, subjectID, subjectType string, ttl time.Duration) (*types.Token, string, error) {
	return r.issue(ctx, "access", subjectID, subjectType, "", ttl, types.TokenKindAccess)
}

// IssueRefreshGrant mints a long-lived refresh grant (RUNE-201). It is NEVER a
// valid request bearer — only the refresh endpoint accepts it. ttl is the
// sliding idle window; 0 means no expiry (discouraged for humans).
func (r *TokenRepo) IssueRefreshGrant(ctx context.Context, name, subjectID, subjectType string, ttl time.Duration) (*types.Token, string, error) {
	return r.issue(ctx, name, subjectID, subjectType, "", ttl, types.TokenKindRefresh)
}

func (r *TokenRepo) issue(ctx context.Context, name, subjectID, subjectType, desc string, ttl time.Duration, kind types.TokenKind) (*types.Token, string, error) {
	secret := newSecret()
	now := time.Now()
	var exp *time.Time
	if ttl > 0 {
		t := now.Add(ttl)
		exp = &t
	}
	tok := &types.Token{
		Name:        name,
		ID:          uuid.NewString(),
		SubjectID:   subjectID,
		SubjectType: subjectType,
		Description: desc,
		IssuedAt:    now,
		ExpiresAt:   exp,
		Revoked:     false,
		SecretHash:  hashSecret(secret),
		Kind:        kind,
	}
	if err := r.st.Create(ctx, types.ResourceTypeToken, "system", tok.ID, tok); err != nil {
		return nil, "", err
	}
	return tok, secret, nil
}

func (r *TokenRepo) Get(ctx context.Context, id string) (*types.Token, error) {
	var t types.Token
	if err := r.st.Get(ctx, types.ResourceTypeToken, "system", id, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *TokenRepo) Revoke(ctx context.Context, id string) error {
	t, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	t.Revoked = true
	return r.st.Update(ctx, types.ResourceTypeToken, "system", t.ID, t)
}

// Update persists changes to an existing token (e.g. refresh-grant rotation).
func (r *TokenRepo) Update(ctx context.Context, t *types.Token) error {
	return r.st.Update(ctx, types.ResourceTypeToken, "system", t.ID, t)
}

// Delete removes a token row outright (used by access-token GC).
func (r *TokenRepo) Delete(ctx context.Context, id string) error {
	return r.st.Delete(ctx, types.ResourceTypeToken, "system", id)
}

func (r *TokenRepo) List(ctx context.Context) ([]types.Token, error) {
	var tokens []types.Token
	if err := r.st.List(ctx, types.ResourceTypeToken, "system", &tokens); err != nil {
		return nil, err
	}
	return tokens, nil
}

// lookupBySecret locates a non-revoked, unexpired token by secret hash,
// regardless of kind. It is private: callers MUST go through FindRequestBearer
// (which rejects refresh tokens) or FindRefreshGrant (which requires them), so
// that no request-authentication path can ever accept a refresh token as a
// bearer (RUNE-201 load-bearing rule).
func (r *TokenRepo) lookupBySecret(ctx context.Context, secret string) (*types.Token, error) {
	secret = strings.TrimSpace(secret)
	// Reject any token that doesn't carry the canonical Rune prefix.
	if !strings.HasPrefix(secret, TokenSecretPrefix) {
		return nil, fmt.Errorf("token not found or invalid")
	}
	// Token namespace is system; list all tokens and match hash (MVP).
	// TODO: index for scalability.
	var tokens []types.Token
	if err := r.st.List(ctx, types.ResourceTypeToken, "system", &tokens); err != nil {
		return nil, err
	}
	h := hashSecret(secret)
	now := time.Now()
	for _, t := range tokens {
		if t.SecretHash == h && !t.Revoked && (t.ExpiresAt == nil || t.ExpiresAt.After(now)) {
			tt := t
			return &tt, nil
		}
	}
	return nil, fmt.Errorf("token not found or invalid")
}

// FindRequestBearer validates a token presented as a request bearer. It rejects
// refresh-kind tokens — they are never valid bearers (RUNE-201). All
// request-authentication paths (gRPC authFunc, dashboard ui middleware, exec WS,
// WhoAmI) MUST use this, never lookupBySecret directly.
func (r *TokenRepo) FindRequestBearer(ctx context.Context, secret string) (*types.Token, error) {
	tok, err := r.lookupBySecret(ctx, secret)
	if err != nil {
		return nil, err
	}
	if tok.Kind == types.TokenKindRefresh {
		// A refresh token is not a bearer. Report as invalid rather than
		// leaking that the secret exists.
		return nil, fmt.Errorf("token not found or invalid")
	}
	return tok, nil
}

// FindRefreshGrant validates a token presented to the refresh endpoint. It
// requires refresh-kind; anything else (access/static) is rejected.
func (r *TokenRepo) FindRefreshGrant(ctx context.Context, secret string) (*types.Token, error) {
	tok, err := r.lookupBySecret(ctx, secret)
	if err != nil {
		return nil, err
	}
	if tok.Kind != types.TokenKindRefresh {
		return nil, fmt.Errorf("token not found or invalid")
	}
	return tok, nil
}

// FindRefreshGrantByPrevHash finds a (non-revoked) refresh grant whose
// PrevSecretHash matches h. Used to detect genuinely stale reuse of an
// already-rotated refresh secret — i.e. theft, past the grace window.
func (r *TokenRepo) FindRefreshGrantByPrevHash(ctx context.Context, secret string) (*types.Token, error) {
	secret = strings.TrimSpace(secret)
	if !strings.HasPrefix(secret, TokenSecretPrefix) {
		return nil, fmt.Errorf("token not found or invalid")
	}
	var tokens []types.Token
	if err := r.st.List(ctx, types.ResourceTypeToken, "system", &tokens); err != nil {
		return nil, err
	}
	h := hashSecret(secret)
	for _, t := range tokens {
		if t.Kind == types.TokenKindRefresh && !t.Revoked && t.PrevSecretHash == h {
			tt := t
			return &tt, nil
		}
	}
	return nil, fmt.Errorf("token not found or invalid")
}

// RotateGrantSecret mints a fresh secret for an existing refresh grant: the
// current secret hash is preserved as PrevSecretHash (for post-grace theft
// detection), the new hash is stored, LastUsedAt is stamped, and the sliding
// idle expiry is extended by slidingTTL (0 = leave expiry unchanged). Returns
// the new plaintext secret once.
func (r *TokenRepo) RotateGrantSecret(ctx context.Context, grant *types.Token, slidingTTL time.Duration) (string, error) {
	newPlain := newSecret()
	now := time.Now()
	grant.PrevSecretHash = grant.SecretHash
	grant.SecretHash = hashSecret(newPlain)
	grant.LastUsedAt = &now
	if slidingTTL > 0 {
		exp := now.Add(slidingTTL)
		grant.ExpiresAt = &exp
	}
	if err := r.Update(ctx, grant); err != nil {
		return "", err
	}
	return newPlain, nil
}

// DeleteExpiredTokens evicts any token whose expiry has passed as of `now`,
// regardless of kind (short-lived access tokens, unused refresh grants that were
// never rotated forward, and static tokens issued with a TTL). Tokens with no
// expiry (ExpiresAt == nil) are kept. Returns the number deleted. Revoked rows
// are intentionally retained for audit (`token list` shows revoked=true).
func (r *TokenRepo) DeleteExpiredTokens(ctx context.Context, now time.Time) (int, error) {
	tokens, err := r.List(ctx)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for i := range tokens {
		t := &tokens[i]
		if t.ExpiresAt != nil && !t.ExpiresAt.After(now) {
			if err := r.Delete(ctx, t.ID); err != nil {
				return deleted, err
			}
			deleted++
		}
	}
	return deleted, nil
}
