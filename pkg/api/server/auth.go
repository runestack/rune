package server

import (
	"context"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/auth"
	"github.com/runestack/rune/pkg/store/repos"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// AuthorizationHeader is the header key for API key authentication.
	AuthorizationHeader = "authorization"

	// APIKeyPrefix is the prefix for API key values in the Authorization header.
	APIKeyPrefix = "Bearer "
)

// authCtxKeyType prevents collisions
type authCtxKeyType string

var authCtxKey authCtxKeyType = "rune-auth"

// AuthInfo holds minimal identity used by handlers for RBAC checks
type AuthInfo struct {
	SubjectID string
}

// authFunc is the authentication function for gRPC requests.
func (s *APIServer) authFunc(ctx context.Context) (context.Context, error) {
	// If auth is disabled, no need to check auth
	if !s.options.EnableAuth {
		return ctx, nil
	}

	// Extract bearer token from metadata
	token, err := auth.AuthFromMD(ctx, "bearer")
	if err != nil {
		s.logger.Warn("Missing bearer token in request")
		return nil, status.Errorf(codes.Unauthenticated, "missing bearer token: %v", err)
	}

	// Validate bearer token via TokenRepo
	tokRepo := repos.NewTokenRepo(s.store)
	tok, err := tokRepo.FindBySecret(ctx, token)
	if err != nil {
		s.logger.Warn("Invalid bearer token")
		return nil, status.Errorf(codes.Unauthenticated, "invalid bearer token")
	}

	// Enrich context with subject only; permissions resolved via policy engine
	info := &AuthInfo{SubjectID: tok.SubjectID}
	ctx = context.WithValue(ctx, authCtxKey, info)
	return ctx, nil
}
