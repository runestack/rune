package service

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/api/session"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// defaultEnrollTTL bounds how long an enrollment code is valid. Short by design:
// the code is a low-value bearer of "the right to create one grant", not a
// credential, so a tight window limits exposure if it leaks in transit.
const defaultEnrollTTL = 10 * time.Minute

// enrollEntry records the intent captured by an enrollment code.
type enrollEntry struct {
	subjectName string
	subjectType string
	policies    []string
	expiry      time.Time
}

// enrollmentStore is the in-memory, single-use, TTL'd store backing the
// RUNE-201 enrollment-code flow. In-memory is acceptable: codes are short-lived
// and a daemon restart simply invalidates pending codes (the admin re-issues).
type enrollmentStore struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]enrollEntry
}

func newEnrollmentStore() *enrollmentStore {
	return &enrollmentStore{now: time.Now, m: make(map[string]enrollEntry)}
}

func (e *enrollmentStore) put(code string, entry enrollEntry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gcLocked()
	e.m[code] = entry
}

// take returns and removes the entry for code (single use). ok is false if the
// code is unknown or expired.
func (e *enrollmentStore) take(code string) (enrollEntry, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.m[code]
	if !ok {
		return enrollEntry{}, false
	}
	delete(e.m, code)
	if e.now().After(entry.expiry) {
		return enrollEntry{}, false
	}
	return entry, true
}

func (e *enrollmentStore) gcLocked() {
	now := e.now()
	for k, v := range e.m {
		if now.After(v.expiry) {
			delete(e.m, k)
		}
	}
}

// newEnrollmentCode mints a short, Slack-pasteable, single-use code. Not a
// long-term secret, but generated from crypto/rand so it can't be guessed
// within its TTL.
func newEnrollmentCode() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return "enr_" + s, nil
}

// Enroll issues a one-time enrollment code (admin-gated via the default
// AuthService RBAC requirement). It validates the requested policies up front so
// a typo fails here rather than at redeem time.
func (s *AuthService) Enroll(ctx context.Context, req *generated.EnrollRequest) (*generated.EnrollResponse, error) {
	subjectType := req.GetSubjectType()
	if subjectType == "" {
		subjectType = "user"
	}
	if subjectType != "user" && subjectType != "service" {
		return nil, status.Errorf(codes.InvalidArgument, "invalid subject-type: %s (expected 'user' or 'service')", subjectType)
	}
	if strings.TrimSpace(req.GetSubjectName()) == "" {
		return nil, status.Error(codes.InvalidArgument, "subject_name is required")
	}
	for _, p := range req.GetPolicies() {
		if p == "" {
			continue
		}
		if _, err := s.policyRepo.Get(ctx, p); err != nil {
			if store.IsNotFoundError(err) {
				return nil, status.Errorf(codes.InvalidArgument, "policy %q not found (use 'rune admin policy list')", p)
			}
			return nil, fmt.Errorf("look up policy %q: %w", p, err)
		}
	}

	ttl := time.Duration(req.GetTtlSeconds()) * time.Second
	if ttl <= 0 {
		ttl = defaultEnrollTTL
	}
	code, err := newEnrollmentCode()
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to generate enrollment code")
	}
	expiry := time.Now().Add(ttl)
	s.enroll.put(code, enrollEntry{
		subjectName: req.GetSubjectName(),
		subjectType: subjectType,
		policies:    req.GetPolicies(),
		expiry:      expiry,
	})
	return &generated.EnrollResponse{Code: code, ExpiresAt: expiry.Unix()}, nil
}

// RedeemEnrollment exchanges a valid code for a freshly minted session. It is
// self-authenticating on the code (exempt from the auth/rbac interceptors): the
// redeemer receives the refresh secret directly, so an admin never handles a
// usable credential.
func (s *AuthService) RedeemEnrollment(ctx context.Context, req *generated.RedeemEnrollmentRequest) (*generated.RedeemEnrollmentResponse, error) {
	entry, ok := s.enroll.take(req.GetCode())
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired enrollment code")
	}

	// Ensure the subject exists and carries the enrolled policies.
	u, err := s.userRepo.GetByNameOrID(ctx, entry.subjectName)
	if store.IsNotFoundError(err) {
		u, err = s.userRepo.Create(ctx, &types.User{Name: entry.subjectName})
	}
	if err != nil {
		return nil, fmt.Errorf("resolve subject: %w", err)
	}
	for _, p := range entry.policies {
		if p == "" {
			continue
		}
		if err := s.ensureUserHasPolicy(ctx, u, p); err != nil {
			return nil, err
		}
	}

	grantName := req.GetGrantName()
	if strings.TrimSpace(grantName) == "" {
		grantName = "enrolled"
	}

	// Mint the refresh grant, bounded by the sliding refresh window so an
	// unused grant eventually expires and is GC'd. If the refresh manager is
	// wired, rotate it immediately so the redeemer gets a ready-to-use access
	// token too; otherwise hand back the grant secret as the refresh token.
	refreshTTL := session.DefaultRefreshTTL
	if s.refresh != nil {
		refreshTTL = s.refresh.RefreshTTL
	}
	_, refreshSecret, err := s.tokenRepo.IssueRefreshGrant(ctx, grantName, u.ID, entry.subjectType, refreshTTL)
	if err != nil {
		return nil, err
	}
	resp := &generated.RedeemEnrollmentResponse{
		RefreshToken: refreshSecret,
		SubjectId:    u.ID,
	}
	if s.refresh != nil {
		out, result := s.refresh.Rotate(ctx, refreshSecret)
		if result == session.ResultOK {
			resp.AccessToken = out.Access
			resp.RefreshToken = out.Refresh
			if out.AccessExp != nil {
				resp.ExpiresAt = out.AccessExp.Unix()
			}
		}
	}
	return resp, nil
}
