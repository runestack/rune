package client

import (
	"context"
	"sync"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// refreshMethod is the gRPC method that mints/rotates sessions. The interceptors
// never try to auto-refresh this call itself (that would recurse).
const refreshMethod = "/rune.api.AuthService/Refresh"

// authState carries the bearer credential for a client connection and, when a
// refresh grant is present, transparently renews the access token on
// Unauthenticated responses (RUNE-201). It implements grpc.PerRPCCredentials so
// the current access token rides every request, and provides unary/stream
// client interceptors that perform single-flight refresh-and-retry.
//
// Two locks, deliberately: tokenMu is a short critical section guarding the
// token strings (taken by GetRequestMetadata on every request). refreshMu
// serializes the refresh operation itself and is held across the network call —
// crucially, GetRequestMetadata does NOT take refreshMu, so the Refresh RPC's
// own credential callback can run while a refresh is in flight. Holding a single
// lock across the call would deadlock (the Refresh RPC re-enters
// GetRequestMetadata).
type authState struct {
	tokenMu      sync.Mutex
	accessToken  string
	refreshToken string

	refreshMu sync.Mutex // serializes refresh(); never held by GetRequestMetadata

	onRefresh func(accessToken, refreshToken string, expiresAt int64) error
	logger    log.Logger
}

func newAuthState(accessToken, refreshToken string, onRefresh func(string, string, int64) error, logger log.Logger) *authState {
	return &authState{
		accessToken:  accessToken,
		refreshToken: refreshToken,
		onRefresh:    onRefresh,
		logger:       logger,
	}
}

// GetRequestMetadata implements grpc.PerRPCCredentials.
func (a *authState) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + a.currentToken()}, nil
}

// RequireTransportSecurity implements grpc.PerRPCCredentials. Local dev allows
// sending the token over plaintext; production should use TLS.
func (a *authState) RequireTransportSecurity() bool { return false }

func (a *authState) currentToken() string {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	return a.accessToken
}

// refresh performs a single-flight token rotation. staleToken is the access
// token the caller saw fail; if another goroutine already rotated past it, this
// returns immediately so concurrent 401s collapse into one refresh. The Refresh
// RPC runs WITHOUT tokenMu held, so its credential callback can read the token.
func (a *authState) refresh(ctx context.Context, cc *grpc.ClientConn, staleToken string) error {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()

	a.tokenMu.Lock()
	cur, rt := a.accessToken, a.refreshToken
	a.tokenMu.Unlock()
	if cur != staleToken {
		return nil // another goroutine already refreshed past the token we used
	}

	resp, err := generated.NewAuthServiceClient(cc).Refresh(ctx, &generated.RefreshRequest{RefreshToken: rt})
	if err != nil {
		return err
	}

	a.tokenMu.Lock()
	a.accessToken = resp.GetAccessToken()
	if r := resp.GetRefreshToken(); r != "" {
		a.refreshToken = r
	}
	newAccess, newRefresh := a.accessToken, a.refreshToken
	a.tokenMu.Unlock()

	if a.onRefresh != nil {
		if perr := a.onRefresh(newAccess, newRefresh, resp.GetExpiresAt()); perr != nil && a.logger != nil {
			a.logger.Warn("failed to persist refreshed session tokens", log.Err(perr))
		}
	}
	return nil
}

// shouldRefresh reports whether an error from method warrants an auto-refresh.
func (a *authState) shouldRefresh(method string, err error) bool {
	if err == nil || method == refreshMethod || status.Code(err) != codes.Unauthenticated {
		return false
	}
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	return a.refreshToken != ""
}

// unaryInterceptor retries a unary call once after refreshing on Unauthenticated.
func (a *authState) unaryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	stale := a.currentToken()
	err := invoker(ctx, method, req, reply, cc, opts...)
	if !a.shouldRefresh(method, err) {
		return err
	}
	if rerr := a.refresh(ctx, cc, stale); rerr != nil {
		return err // surface the original Unauthenticated, not the refresh failure
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

// streamInterceptor refreshes and re-opens a stream once if creation fails with
// Unauthenticated. (Auth errors that only surface on the first Recv are not
// retried here; stream RPCs reject at header time when the bearer is invalid.)
func (a *authState) streamInterceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	stale := a.currentToken()
	s, err := streamer(ctx, desc, cc, method, opts...)
	if !a.shouldRefresh(method, err) {
		return s, err
	}
	if rerr := a.refresh(ctx, cc, stale); rerr != nil {
		return s, err
	}
	return streamer(ctx, desc, cc, method, opts...)
}
