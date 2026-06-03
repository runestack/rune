package repos

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/store"
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

	_, staticSecret, err := repo.IssueStatic(ctx, "ci", "alice", "user", "", 0)
	if err != nil {
		t.Fatalf("issue static: %v", err)
	}
	_, accessSecret, err := repo.IssueAccess(ctx, "alice", "user", time.Hour)
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}
	_, refreshSecret, err := repo.IssueRefreshGrant(ctx, "laptop", "alice", "user", time.Hour)
	if err != nil {
		t.Fatalf("issue refresh: %v", err)
	}

	// Static + access are valid bearers.
	if _, err := repo.FindRequestBearer(ctx, staticSecret); err != nil {
		t.Errorf("static token should be a valid bearer, got %v", err)
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
	if _, err := repo.FindRefreshGrant(ctx, staticSecret); err == nil {
		t.Error("static token must not be accepted as a refresh grant")
	}
}

func TestDeleteExpiredTokens(t *testing.T) {
	ctx := context.Background()
	repo, _ := newTestRepo(t)

	past := time.Now().Add(-time.Hour)

	// Expired access token (deleted).
	expAccess, _, _ := repo.IssueAccess(ctx, "alice", "user", time.Hour)
	expAccess.ExpiresAt = &past
	if err := repo.Update(ctx, expAccess); err != nil {
		t.Fatalf("update: %v", err)
	}
	// Expired (abandoned) refresh grant (deleted).
	expRefresh, _, _ := repo.IssueRefreshGrant(ctx, "g", "alice", "user", time.Hour)
	expRefresh.ExpiresAt = &past
	_ = repo.Update(ctx, expRefresh)

	// Live access (kept) and a no-expiry static token (kept).
	_, _, _ = repo.IssueAccess(ctx, "alice", "user", time.Hour)
	_, _, _ = repo.IssueStatic(ctx, "ci", "alice", "user", "", 0)

	n, err := repo.DeleteExpiredTokens(ctx, time.Now())
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected to delete 2 expired tokens (access + abandoned grant), deleted %d", n)
	}
	all, _ := repo.List(ctx)
	if len(all) != 2 {
		t.Fatalf("expected 2 tokens remaining (live access, no-expiry static), got %d", len(all))
	}
}
