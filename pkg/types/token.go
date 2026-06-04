package types

import "time"

// TokenKind discriminates the role a token plays (RUNE-201). Every token is
// issued with an explicit kind.
type TokenKind string

const (
	// TokenKindStatic is a long-lived bearer token with no refresh: the model
	// service accounts, CI (`cast`), and the bootstrap/break-glass root token
	// use. Accepted as a request bearer.
	TokenKindStatic TokenKind = "static"

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

	// Kind discriminates static/access/refresh (RUNE-201).
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
