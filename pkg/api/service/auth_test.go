package service

import (
	"context"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/api/session"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
)

func newAuthTestService(t *testing.T) (*AuthService, store.Store) {
	t.Helper()
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	// A readonly policy so the auto-create path has something valid to attach.
	if err := repos.NewPolicyRepo(st).Create(ctx, &types.Policy{Name: "readonly"}); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	return NewAuthService(st, log.GetDefaultLogger()), st
}

// CreateToken with kind=refresh must produce a genuine refresh grant: rejected
// as a request bearer, accepted by the refresh-grant lookup. This is the
// human-facing issuance path that makes RUNE-201 reachable end-to-end.
func TestCreateToken_RefreshKind(t *testing.T) {
	ctx := context.Background()
	svc, st := newAuthTestService(t)
	repo := repos.NewTokenRepo(st)

	resp, err := svc.CreateToken(ctx, &generated.CreateTokenRequest{
		Name:        "alice-laptop",
		SubjectName: "alice",
		SubjectType: "user",
		Kind:        "refresh",
	})
	if err != nil {
		t.Fatalf("create refresh grant: %v", err)
	}
	if resp.Secret == "" {
		t.Fatal("no secret returned")
	}

	// The issued credential is a refresh grant, not a bearer.
	if _, err := repo.FindRequestBearer(ctx, resp.Secret); err == nil {
		t.Fatal("SECURITY: issued refresh grant was accepted as a request bearer")
	}
	grant, err := repo.FindRefreshGrant(ctx, resp.Secret)
	if err != nil {
		t.Fatalf("refresh grant should be accepted by FindRefreshGrant: %v", err)
	}
	if grant.EffectiveKind() != types.TokenKindRefresh {
		t.Fatalf("expected refresh kind, got %q", grant.EffectiveKind())
	}
}

func TestCreateToken_LegacyDefault(t *testing.T) {
	ctx := context.Background()
	svc, st := newAuthTestService(t)
	repo := repos.NewTokenRepo(st)

	resp, err := svc.CreateToken(ctx, &generated.CreateTokenRequest{
		SubjectName: "svc-ci",
		SubjectType: "service",
		// kind unset → legacy
	})
	if err != nil {
		t.Fatalf("create legacy token: %v", err)
	}
	if _, err := repo.FindRequestBearer(ctx, resp.Secret); err != nil {
		t.Fatalf("legacy token should be a valid bearer: %v", err)
	}
}

// End-to-end gRPC path: issue a refresh grant, then exchange it via the
// self-authenticating Refresh RPC for a usable access token + rotated refresh.
func TestRefreshRPC_EndToEnd(t *testing.T) {
	ctx := context.Background()
	svc, st := newAuthTestService(t)
	svc.SetRefreshManager(session.New(st, log.GetDefaultLogger()))
	repo := repos.NewTokenRepo(st)

	created, err := svc.CreateToken(ctx, &generated.CreateTokenRequest{
		SubjectName: "alice", SubjectType: "user", Kind: "refresh",
	})
	if err != nil {
		t.Fatalf("create refresh grant: %v", err)
	}

	resp, err := svc.Refresh(ctx, &generated.RefreshRequest{RefreshToken: created.Secret})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if resp.AccessToken == "" || resp.RefreshToken == "" {
		t.Fatalf("refresh must return both tokens, got %+v", resp)
	}
	if resp.RefreshToken == created.Secret {
		t.Fatal("refresh token was not rotated")
	}
	// access token is a bearer; rotated refresh is not.
	if _, err := repo.FindRequestBearer(ctx, resp.AccessToken); err != nil {
		t.Fatalf("access token should be a valid bearer: %v", err)
	}
	if _, err := repo.FindRequestBearer(ctx, resp.RefreshToken); err == nil {
		t.Fatal("SECURITY: rotated refresh token accepted as a bearer")
	}
}

func TestRefreshRPC_InvalidToken(t *testing.T) {
	ctx := context.Background()
	svc, st := newAuthTestService(t)
	svc.SetRefreshManager(session.New(st, log.GetDefaultLogger()))
	if _, err := svc.Refresh(ctx, &generated.RefreshRequest{RefreshToken: "rune_nope"}); err == nil {
		t.Fatal("invalid refresh token should error")
	}
}

func TestRefreshRPC_NotEnabled(t *testing.T) {
	ctx := context.Background()
	svc, _ := newAuthTestService(t) // no SetRefreshManager
	if _, err := svc.Refresh(ctx, &generated.RefreshRequest{RefreshToken: "rune_x"}); err == nil {
		t.Fatal("refresh without a manager should return an error")
	}
}

// Enroll → Redeem is the Phase 2 flow: an admin-issued code lets the user
// self-provision a session; the refresh secret is returned to the redeemer, and
// the code is single-use.
func TestEnrollThenRedeem(t *testing.T) {
	ctx := context.Background()
	svc, st := newAuthTestService(t)
	svc.SetRefreshManager(session.New(st, log.GetDefaultLogger()))
	repo := repos.NewTokenRepo(st)

	enr, err := svc.Enroll(ctx, &generated.EnrollRequest{
		SubjectName: "alice", SubjectType: "user", Policies: []string{"readonly"},
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if enr.Code == "" || enr.ExpiresAt == 0 {
		t.Fatalf("enroll must return a code + expiry, got %+v", enr)
	}

	red, err := svc.RedeemEnrollment(ctx, &generated.RedeemEnrollmentRequest{Code: enr.Code, GrantName: "alice-laptop"})
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if red.AccessToken == "" || red.RefreshToken == "" || red.SubjectId == "" {
		t.Fatalf("redeem must return access+refresh+subject, got %+v", red)
	}
	// access token is a usable bearer; refresh is a grant, not a bearer.
	if _, err := repo.FindRequestBearer(ctx, red.AccessToken); err != nil {
		t.Fatalf("redeemed access token should be a valid bearer: %v", err)
	}
	if _, err := repo.FindRefreshGrant(ctx, red.RefreshToken); err != nil {
		t.Fatalf("redeemed refresh token should be a valid grant: %v", err)
	}
	// the subject was created with the enrolled policy.
	u, err := repos.NewUserRepo(st).GetByNameOrID(ctx, "alice")
	if err != nil {
		t.Fatalf("subject should exist after redeem: %v", err)
	}
	if len(u.Policies) == 0 || u.Policies[0] != "readonly" {
		t.Fatalf("subject should carry the enrolled policy, got %v", u.Policies)
	}

	// Single-use: the same code cannot be redeemed twice.
	if _, err := svc.RedeemEnrollment(ctx, &generated.RedeemEnrollmentRequest{Code: enr.Code}); err == nil {
		t.Fatal("enrollment code must be single-use")
	}
}

func TestRedeem_InvalidCode(t *testing.T) {
	ctx := context.Background()
	svc, _ := newAuthTestService(t)
	if _, err := svc.RedeemEnrollment(ctx, &generated.RedeemEnrollmentRequest{Code: "enr_nope"}); err == nil {
		t.Fatal("unknown enrollment code should error")
	}
}

func TestEnroll_ExpiredCodeRejected(t *testing.T) {
	ctx := context.Background()
	svc, _ := newAuthTestService(t)
	enr, err := svc.Enroll(ctx, &generated.EnrollRequest{SubjectName: "bob", Policies: []string{"readonly"}})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// Force the stored code to be expired.
	svc.enroll.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := svc.RedeemEnrollment(ctx, &generated.RedeemEnrollmentRequest{Code: enr.Code}); err == nil {
		t.Fatal("expired enrollment code should be rejected")
	}
}

func TestEnroll_InvalidPolicy(t *testing.T) {
	ctx := context.Background()
	svc, _ := newAuthTestService(t)
	if _, err := svc.Enroll(ctx, &generated.EnrollRequest{SubjectName: "alice", Policies: []string{"nope"}}); err == nil {
		t.Fatal("enroll with a nonexistent policy should error")
	}
}

func TestCreateToken_RejectsAccessKind(t *testing.T) {
	ctx := context.Background()
	svc, _ := newAuthTestService(t)
	if _, err := svc.CreateToken(ctx, &generated.CreateTokenRequest{
		SubjectName: "alice", Kind: "access",
	}); err == nil {
		t.Fatal("access kind must not be directly issuable via CreateToken")
	}
}
