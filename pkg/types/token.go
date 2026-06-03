package types

import "time"

// TokenKind discriminates the role a token plays (RUNE-201). The zero value
// ("") is treated as Legacy for backwards compatibility with tokens issued
// before this field existed — see Token.EffectiveKind.
type TokenKind string

const (
	// TokenKindLegacy is a long-lived bearer token (the pre-RUNE-201 model,
	// and the model service accounts / CI continue to use). Accepted as a
	// request bearer; never refreshed.
	TokenKindLegacy TokenKind = "legacy"

	// TokenKindAccess is a short-lived session credential minted from a
	// refresh grant. Accepted as a request bearer; expires quickly.
	TokenKindAccess TokenKind = "access"

	// TokenKindRefresh is a long-lived grant secret. It is NEVER a valid
	// request bearer — it is accepted only by the refresh endpoint. This is
	// the load-bearing rule of RUNE-201: if a refresh token were accepted as
	// a bearer, the short-lived-access design collapses.
	TokenKindRefresh TokenKind = "refresh"
)

// Token represents an authentication token (opaque secret stored hashed)
type Token struct {
	ID          string     `json:"id" yaml:"id"`
	Name        string     `json:"name" yaml:"name"`
	SubjectID   string     `json:"subjectId" yaml:"subjectId"`
	SubjectType string     `json:"subjectType" yaml:"subjectType"` // "user" | "service"
	Description string     `json:"description,omitempty" yaml:"description,omitempty"`
	IssuedAt    time.Time  `json:"issuedAt" yaml:"issuedAt"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty" yaml:"expiresAt,omitempty"`
	Revoked     bool       `json:"revoked" yaml:"revoked"`
	SecretHash  string     `json:"secretHash" yaml:"secretHash"`

	// Kind discriminates legacy/access/refresh (RUNE-201). Empty means legacy
	// (tokens predating the field); always read via EffectiveKind.
	Kind TokenKind `json:"kind,omitempty" yaml:"kind,omitempty"`

	// PrevSecretHash holds the hash of the immediately-prior refresh secret for
	// a refresh grant, set on rotation. It exists so a genuinely stale (post
	// grace-window) reuse of a rotated refresh secret can be identified and
	// treated as theft. Only meaningful when Kind==refresh.
	PrevSecretHash string `json:"prevSecretHash,omitempty" yaml:"prevSecretHash,omitempty"`

	// LastUsedAt records the last time a refresh grant was rotated (RUNE-201
	// sliding expiry). Only meaningful when Kind==refresh.
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty" yaml:"lastUsedAt,omitempty"`
}

func (t *Token) GetID() string                 { return t.ID }
func (t *Token) GetResourceType() ResourceType { return ResourceTypeToken }

// EffectiveKind normalizes the zero value to Legacy. Every reader MUST use this
// rather than comparing Kind directly, so that tokens issued before the field
// existed (Kind=="") are treated as legacy bearers and keep working across the
// upgrade. The safety invariant elsewhere is: only EffectiveKind in
// {Access, Legacy} is accepted as a request bearer.
func (t *Token) EffectiveKind() TokenKind {
	if t.Kind == "" {
		return TokenKindLegacy
	}
	return t.Kind
}
