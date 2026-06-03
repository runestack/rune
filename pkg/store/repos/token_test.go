package repos

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

func newTestRepo(t *testing.T) (*TokenRepo, store.Store) {
	t.Helper()
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open store: %v", err)
	}
	return NewTokenRepo(st), st
}

// The load-bearing RUNE-201 rule: a refresh token is NEVER a valid request
// bearer, and only a refresh token is accepted by the refresh-grant lookup.
func TestFindRequestBearer_RejectsRefresh(t *testing.T) {
	ctx := context.Background()
	repo, _ := newTestRepo(t)

	_, legacySecret, err := repo.Issue(ctx, "legacy", "alice", "user", "", 0)
	if err != nil {
		t.Fatalf("issue legacy: %v", err)
	}
	_, accessSecret, err := repo.IssueAccess(ctx, "alice", "user", time.Hour)
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}
	_, refreshSecret, err := repo.IssueRefreshGrant(ctx, "laptop", "alice", "user", time.Hour)
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}

	// Legacy + access are valid bearers.
	if _, err := repo.FindRequestBearer(ctx, legacySecret); err != nil {
		t.Errorf("legacy token should be a valid bearer, got %v", err)
	}
	if _, err := repo.FindRequestBearer(ctx, accessSecret); err != nil {
		t.Errorf("access token should be a valid bearer, got %v", err)
	}
	// Refresh is NOT a valid bearer — this is the whole point.
	if _, err := repo.FindRequestBearer(ctx, refreshSecret); err == nil {
		t.Fatal("SECURITY: refresh token was accepted as a request bearer")
	}

	// Conversely, only refresh is accepted by FindRefreshGrant.
	if _, err := repo.FindRefreshGrant(ctx, refreshSecret); err != nil {
		t.Errorf("refresh grant lookup should accept refresh token, got %v", err)
	}
	if _, err := repo.FindRefreshGrant(ctx, accessSecret); err == nil {
		t.Error("access token must not be accepted as a refresh grant")
	}
	if _, err := repo.FindRefreshGrant(ctx, legacySecret); err == nil {
		t.Error("legacy token must not be accepted as a refresh grant")
	}
}

// A token persisted before the Kind field existed (Kind=="") must be treated as
// legacy and remain a valid bearer across the upgrade.
func TestEmptyKindIsLegacyBearer(t *testing.T) {
	ctx := context.Background()
	repo, st := newTestRepo(t)

	secret := newSecret()
	tok := &types.Token{
		ID:         uuid.NewString(),
		Name:       "preupgrade",
		SubjectID:  "alice",
		IssuedAt:   time.Now(),
		SecretHash: hashSecret(secret),
		// Kind intentionally unset to simulate a pre-RUNE-201 row.
	}
	if err := st.Create(ctx, types.ResourceTypeToken, "system", tok.ID, tok); err != nil {
		t.Fatalf("seed legacy token: %v", err)
	}
	if tok.EffectiveKind() != types.TokenKindLegacy {
		t.Fatalf("empty kind should normalize to legacy, got %q", tok.EffectiveKind())
	}
	if _, err := repo.FindRequestBearer(ctx, secret); err != nil {
		t.Fatalf("pre-upgrade (kind=\"\") token must remain a valid bearer, got %v", err)
	}
}

func TestDeleteExpiredAccessTokens(t *testing.T) {
	ctx := context.Background()
	repo, _ := newTestRepo(t)

	// One expired access token, one live, one legacy (exempt), one refresh (exempt).
	expTok, _, _ := repo.IssueAccess(ctx, "alice", "user", time.Hour)
	// force-expire it
	past := time.Now().Add(-time.Hour)
	expTok.ExpiresAt = &past
	if err := repo.Update(ctx, expTok); err != nil {
		t.Fatalf("update: %v", err)
	}
	_, _, _ = repo.IssueAccess(ctx, "alice", "user", time.Hour) // live
	_, _, _ = repo.Issue(ctx, "legacy", "alice", "user", "", 0) // exempt
	expRefresh, _, _ := repo.IssueRefreshGrant(ctx, "g", "alice", "user", 0)
	expRefresh.ExpiresAt = &past // even an "expired" refresh is exempt from access GC
	_ = repo.Update(ctx, expRefresh)

	n, err := repo.DeleteExpiredAccessTokens(ctx, time.Now())
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected to delete exactly 1 expired access token, deleted %d", n)
	}
	all, _ := repo.List(ctx)
	if len(all) != 3 {
		t.Fatalf("expected 3 tokens remaining (live access, legacy, refresh), got %d", len(all))
	}
}
