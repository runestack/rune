// Package session implements RUNE-201 refresh-token rotation with a grace
// window. It is transport-neutral: both the browser-facing HTTP cookie endpoint
// (pkg/api/server) and the CLI-facing gRPC AuthService.Refresh RPC
// (pkg/api/service) drive the same Manager instance, so the in-memory grace
// cache is shared across both surfaces. Living in its own package avoids a
// service↔server import cycle.
package session

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
)

// RUNE-201 session parameters. Defaults; promoted to config later if needed.
const (
	DefaultAccessTTL   = 15 * time.Minute
	DefaultRefreshTTL  = 30 * 24 * time.Hour // sliding idle window
	DefaultGraceWindow = 30 * time.Second    // tolerate concurrent-refresh races
)

// Result classifies the outcome of a refresh attempt.
type Result int

const (
	ResultOK      Result = iota // rotated (or served a within-grace successor)
	ResultBreach                // stale reuse past grace → grant revoked (theft)
	ResultInvalid               // unknown/expired/non-refresh secret
	ResultError                 // internal error
)

// Out carries the credentials handed back to the caller.
type Out struct {
	Access    string
	Refresh   string
	AccessExp *time.Time
}

// graceEntry caches the successor credentials issued for a just-rotated secret,
// keyed by the OLD secret's hash, so concurrent racers presenting the same old
// secret within the grace window all converge on the same successor instead of
// tripping the theft alarm. Necessarily in-memory (we only persist hashes, so a
// rotated secret's plaintext can't be re-derived); lost on restart, which at
// worst forces a stale racer onto the post-grace breach path.
type graceEntry struct {
	out      Out
	graceExp time.Time
}

// Manager implements refresh-token rotation with a grace window. Rotations are
// serialized by mu to keep the cache and the store consistent (single-server
// MVP; refresh volume is low).
type Manager struct {
	store       store.Store
	logger      log.Logger
	AccessTTL   time.Duration
	RefreshTTL  time.Duration
	GraceWindow time.Duration
	now         func() time.Time

	mu    sync.Mutex
	grace map[string]graceEntry
}

// New constructs a Manager with default TTLs.
func New(st store.Store, logger log.Logger) *Manager {
	if logger == nil {
		logger = log.GetDefaultLogger()
	}
	return &Manager{
		store:       st,
		logger:      logger.WithComponent("session"),
		AccessTTL:   DefaultAccessTTL,
		RefreshTTL:  DefaultRefreshTTL,
		GraceWindow: DefaultGraceWindow,
		now:         time.Now,
		grace:       make(map[string]graceEntry),
	}
}

// Rotate validates the presented refresh secret and returns fresh credentials.
// See Result for the outcome classification.
func (m *Manager) Rotate(ctx context.Context, secret string) (Out, Result) {
	m.mu.Lock()
	defer m.mu.Unlock()

	repo := repos.NewTokenRepo(m.store)
	now := m.now()

	// 1. Current secret of a live grant → legitimate rotation.
	if grant, err := repo.FindRefreshGrant(ctx, secret); err == nil {
		access, accessExp, err := m.issueAccess(ctx, repo, grant.SubjectID, grant.SubjectType)
		if err != nil {
			m.logger.Error("refresh: issue access token", log.Err(err))
			return Out{}, ResultError
		}
		newRefresh, err := repo.RotateGrantSecret(ctx, grant, m.RefreshTTL)
		if err != nil {
			m.logger.Error("refresh: rotate grant secret", log.Err(err))
			return Out{}, ResultError
		}
		out := Out{Access: access, Refresh: newRefresh, AccessExp: accessExp}
		m.grace[repos.HashSecret(strings.TrimSpace(secret))] = graceEntry{out: out, graceExp: now.Add(m.GraceWindow)}
		m.gcGraceLocked(now)
		return out, ResultOK
	}

	// 2. Within-grace replay of a just-rotated secret → return the successor.
	h := repos.HashSecret(strings.TrimSpace(secret))
	if e, ok := m.grace[h]; ok {
		if now.Before(e.graceExp) {
			return e.out, ResultOK
		}
		delete(m.grace, h) // expired; fall through to breach detection
	}

	// 3. Stale reuse of a rotated secret past grace → theft → revoke the grant.
	if g, err := repo.FindRefreshGrantByPrevHash(ctx, secret); err == nil {
		if rerr := repo.Revoke(ctx, g.ID); rerr != nil {
			m.logger.Error("refresh: revoke grant on reuse", log.Err(rerr))
		}
		m.logger.Warn("refresh token reuse detected past grace; grant revoked",
			log.Str("grant_id", g.ID), log.Str("subject_id", g.SubjectID))
		return Out{}, ResultBreach
	}

	// 4. Unknown / expired / non-refresh secret.
	return Out{}, ResultInvalid
}

func (m *Manager) issueAccess(ctx context.Context, repo *repos.TokenRepo, subjectID, subjectType string) (string, *time.Time, error) {
	tok, secret, err := repo.IssueAccess(ctx, subjectID, subjectType, m.AccessTTL)
	if err != nil {
		return "", nil, err
	}
	return secret, tok.ExpiresAt, nil
}

// gcGraceLocked drops expired grace entries. Caller holds m.mu.
func (m *Manager) gcGraceLocked(now time.Time) {
	for k, e := range m.grace {
		if now.After(e.graceExp) {
			delete(m.grace, k)
		}
	}
}
