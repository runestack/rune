package service

import (
	"context"
	"fmt"
	"time"

	grpc_auth "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/auth"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

// AuthService implements generated.AuthServiceServer
type AuthService struct {
	generated.UnimplementedAuthServiceServer
	tokenRepo  *repos.TokenRepo
	userRepo   *repos.UserRepo
	policyRepo *repos.PolicyRepo
	logger     log.Logger
}

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
		logger:     logger,
	}
}

// AuthService implementation
func (s *AuthService) WhoAmI(ctx context.Context, _ *generated.WhoAmIRequest) (*generated.WhoAmIResponse, error) {
	resp := &generated.WhoAmIResponse{}

	// Re-extract bearer and look up token to avoid relying on server context types
	if token, err := grpc_auth.AuthFromMD(ctx, "bearer"); err == nil {
		if tok, err2 := s.tokenRepo.FindBySecret(ctx, token); err2 == nil {
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
	// Determine subject type (users only for MVP, services planned for future)
	subjectType := req.SubjectType
	if subjectType == "" {
		subjectType = "user"
	}

	// TODO: Implement service token creation when service types are added
	if subjectType != "user" {
		return nil, fmt.Errorf("invalid subject-type: %s (only 'user' supported for now, service support planned)", subjectType)
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

	// Issue token
	tok, secret, err := s.tokenRepo.Issue(ctx, req.Name, u.ID, subjectType, req.Description, time.Duration(req.TtlSeconds)*time.Second)
	if err != nil {
		return nil, err
	}
	return &generated.CreateTokenResponse{Id: tok.ID, Name: tok.Name, Secret: secret}, nil
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
