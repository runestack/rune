package service

import (
	"context"
	"fmt"
	"time"

	grpc_auth "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/auth"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/api/session"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AuthService implements generated.AuthServiceServer
type AuthService struct {
	generated.UnimplementedAuthServiceServer
	tokenRepo  *repos.TokenRepo
	userRepo   *repos.UserRepo
	policyRepo *repos.PolicyRepo
	refresh    *session.Manager
	enroll     *enrollmentStore
	logger     log.Logger
}

// SetRefreshManager wires the shared RUNE-201 rotation manager. Injected by the
// server after construction so the gRPC Refresh RPC and the HTTP cookie endpoint
// share one in-memory grace cache. When nil, Refresh returns Unimplemented.
func (s *AuthService) SetRefreshManager(m *session.Manager) { s.refresh = m }

func NewAuthService(st store.Store, logger log.Logger) *AuthService {
	if logger == nil {
		logger = log.GetDefaultLogger().WithComponent("auth-service")
	} else {
		logger = logger.WithComponent("auth-service")
	}
	return &AuthService{
		tokenRepo:  repos.NewTokenRepo(st),
		userRepo:   repos.NewUserRepo(st),
		policyRepo: repos.NewPolicyRepo(st),
		enroll:     newEnrollmentStore(),
		logger:     logger,
	}
}

// AuthService implementation
func (s *AuthService) WhoAmI(ctx context.Context, _ *generated.WhoAmIRequest) (*generated.WhoAmIResponse, error) {
	resp := &generated.WhoAmIResponse{}

	// Re-extract bearer and look up token to avoid relying on server context types
	if token, err := grpc_auth.AuthFromMD(ctx, "bearer"); err == nil {
		if tok, err2 := s.tokenRepo.FindRequestBearer(ctx, token); err2 == nil {
			resp.SubjectId = tok.SubjectID

			// Look up the actual subject details based on SubjectType
			switch tok.SubjectType {
			case "user":
				// Look up user details
				if user, err := s.userRepo.GetByNameOrID(ctx, tok.SubjectID); err == nil {
					resp.SubjectName = user.Name
					resp.SubjectEmail = user.Email
					resp.Policies = user.Policies
				} else {
					s.logger.Warn("Failed to look up user details", log.F("subject_id", tok.SubjectID), log.Err(err))
				}

			case "service":
				// TODO: Implement service lookup when service types are added
				// For now, just set the subject name from token name
				resp.SubjectName = tok.Name
				s.logger.Debug("Service token detected, service lookup not yet implemented", log.F("subject_id", tok.SubjectID))

			default:
				s.logger.Warn("Unknown subject type", log.F("subject_type", tok.SubjectType), log.F("subject_id", tok.SubjectID))
			}
		}
	}

	return resp, nil
}

func (s *AuthService) CreateToken(ctx context.Context, req *generated.CreateTokenRequest) (*generated.CreateTokenResponse, error) {
	// Determine subject type. We support "user" (humans) and
	// "service" (CI / automation). Both authenticate identically;
	// the type is a label for auditing and listing.
	subjectType := req.SubjectType
	if subjectType == "" {
		subjectType = "user"
	}
	if subjectType != "user" && subjectType != "service" {
		return nil, fmt.Errorf("invalid subject-type: %s (expected 'user' or 'service')", subjectType)
	}

	// Validate the credential kind (RUNE-201). Only static (default) and refresh
	// are issuable here; access tokens are minted exclusively by the refresh
	// endpoint, never directly.
	kind := types.TokenKind(req.GetKind())
	switch kind {
	case "", types.TokenKindStatic:
		kind = types.TokenKindStatic
	case types.TokenKindRefresh:
		// ok
	default:
		return nil, fmt.Errorf("invalid kind: %s (expected 'static' or 'refresh')", req.GetKind())
	}

	// Validate every requested policy exists *before* mutating any
	// state. Without this guard a typo (e.g. --policy deployer when
	// no such policy was defined) silently issues a token whose
	// subject ends up with an unresolvable policy reference; every
	// subsequent RPC then fails with PermissionDenied and the user
	// has no way to tell that the root cause was at create-time.
	for _, p := range req.Policies {
		if p == "" {
			continue
		}
		if _, err := s.policyRepo.Get(ctx, p); err != nil {
			if store.IsNotFoundError(err) {
				return nil, fmt.Errorf("policy %q not found (use 'rune admin policy list' to see available policies)", p)
			}
			return nil, fmt.Errorf("look up policy %q: %w", p, err)
		}
	}

	// Resolve identifier for subject: prefer SubjectId, else use subject name or token name
	nameOrID := utils.PickFirstNonEmpty(req.SubjectId, req.SubjectName, req.Name)
	if nameOrID == "" {
		return nil, fmt.Errorf("either subject_name or subject_id must be provided to derive subject")
	}

	// Ensure user exists (create if missing); attach default policies on auto-create
	u, err := s.userRepo.GetByNameOrID(ctx, nameOrID)
	if store.IsNotFoundError(err) {
		// Resolve subject name: prefer SubjectId (treated as user name), else use token Name
		subjectName := utils.PickFirstNonEmpty(req.SubjectName, req.Name)
		if subjectName == "" {
			return nil, fmt.Errorf("either subject_name or name must be provided to create subject")
		}
		createdUser, err := s.userRepo.Create(ctx, &types.User{Name: subjectName})
		if err != nil {
			return nil, err
		}
		// set newly created user to u
		u = createdUser
		// Attach provided policies; fallback to readonly if none provided
		attached := false
		if len(req.Policies) > 0 {
			for _, p := range req.Policies {
				if p == "" {
					continue
				}
				if err := s.ensureUserHasPolicy(ctx, u, p); err != nil {
					return nil, err
				}
				attached = true
			}
		}
		if !attached {
			if err := s.ensureUserHasPolicy(ctx, u, "readonly"); err != nil {
				return nil, err
			}
		}
	}

	// Issue the credential. Refresh grants are exchanged at /v1/auth/refresh for
	// short-lived access tokens; legacy tokens are direct long-lived bearers.
	ttl := time.Duration(req.TtlSeconds) * time.Second
	var (
		tok    *types.Token
		secret string
	)
	if kind == types.TokenKindRefresh {
		// A refresh grant with no caller TTL gets the sliding refresh window so
		// it isn't a permanent credential; rotation extends it on use.
		if ttl <= 0 {
			ttl = session.DefaultRefreshTTL
		}
		tok, secret, err = s.tokenRepo.IssueRefreshGrant(ctx, req.Name, u.ID, subjectType, ttl)
	} else {
		tok, secret, err = s.tokenRepo.IssueStatic(ctx, req.Name, u.ID, subjectType, req.Description, ttl)
	}
	if err != nil {
		return nil, err
	}
	return &generated.CreateTokenResponse{Id: tok.ID, Name: tok.Name, Secret: secret}, nil
}

// Refresh exchanges a refresh grant for a fresh access token and a rotated
// refresh token (RUNE-201). It is self-authenticating on the refresh secret —
// the auth/rbac interceptors exempt this method — so any grant holder can renew
// their own session, and only their own.
func (s *AuthService) Refresh(ctx context.Context, req *generated.RefreshRequest) (*generated.RefreshResponse, error) {
	if s.refresh == nil {
		return nil, status.Error(codes.Unimplemented, "session refresh not enabled")
	}
	out, result := s.refresh.Rotate(ctx, req.GetRefreshToken())
	switch result {
	case session.ResultOK:
		resp := &generated.RefreshResponse{AccessToken: out.Access, RefreshToken: out.Refresh}
		if out.AccessExp != nil {
			resp.ExpiresAt = out.AccessExp.Unix()
		}
		return resp, nil
	case session.ResultBreach:
		return nil, status.Error(codes.Unauthenticated, "refresh token reuse detected; session revoked")
	case session.ResultInvalid:
		return nil, status.Error(codes.Unauthenticated, "invalid refresh token")
	default:
		return nil, status.Error(codes.Internal, "refresh failed")
	}
}

func (s *AuthService) RevokeToken(ctx context.Context, req *generated.RevokeTokenRequest) (*generated.RevokeTokenResponse, error) {
	if err := s.tokenRepo.Revoke(ctx, req.Id); err != nil {
		return nil, err
	}
	return &generated.RevokeTokenResponse{Revoked: true}, nil
}

// ensureUserHasPolicy attaches policyName to the user if it's not already present
func (s *AuthService) ensureUserHasPolicy(ctx context.Context, u *types.User, policyName string) error {
	for _, p := range u.Policies {
		if p == policyName {
			return nil
		}
	}
	u.Policies = append(u.Policies, policyName)
	return s.userRepo.Update(ctx, u)
}
