package client_test

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/api/service"
	"github.com/runestack/rune/pkg/api/session"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// End-to-end: a client holding only a refresh grant (no access token) makes a
// protected call, gets Unauthenticated, transparently refreshes via the gRPC
// Refresh RPC, persists the rotated tokens, and the retried call succeeds.
func TestClientAutoRefresh(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open store: %v", err)
	}
	repo := repos.NewTokenRepo(st)

	// Issue a refresh grant; the client starts with only this.
	_, grantSecret, err := repo.IssueRefreshGrant(ctx, "laptop", "alice", "user", time.Hour)
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}

	authSvc := service.NewAuthService(st, log.GetDefaultLogger())
	authSvc.SetRefreshManager(session.New(st, log.GetDefaultLogger()))

	// Server gate: everything except Refresh requires a valid request bearer —
	// mirroring production (FindRequestBearer rejects refresh tokens too).
	gate := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == "/rune.api.AuthService/Refresh" {
			return handler(ctx, req)
		}
		var tok string
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if v := md.Get("authorization"); len(v) > 0 {
				tok = strings.TrimPrefix(v[0], "Bearer ")
			}
		}
		if _, err := repo.FindRequestBearer(ctx, tok); err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid bearer")
		}
		return handler(ctx, req)
	}

	srv := grpc.NewServer(grpc.UnaryInterceptor(gate))
	generated.RegisterAuthServiceServer(srv, authSvc)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	var mu sync.Mutex
	var persistedAccess, persistedRefresh string
	c, err := client.NewClient(&client.ClientOptions{
		Address:      lis.Addr().String(),
		RefreshToken: grantSecret, // no access token yet
		DialTimeout:  5 * time.Second,
		CallTimeout:  5 * time.Second,
		Logger:       log.GetDefaultLogger(),
		OnRefresh: func(access, refresh string, _ int64) error {
			mu.Lock()
			persistedAccess, persistedRefresh = access, refresh
			mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer c.Close()

	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// First protected call: empty bearer → 401 → auto-refresh → retry succeeds.
	if _, err := generated.NewAuthServiceClient(c.Conn()).WhoAmI(callCtx, &generated.WhoAmIRequest{}); err != nil {
		t.Fatalf("WhoAmI after auto-refresh should succeed, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if persistedAccess == "" {
		t.Fatal("OnRefresh was not invoked with a new access token")
	}
	if persistedRefresh == "" || persistedRefresh == grantSecret {
		t.Fatalf("refresh token should have rotated, got %q", persistedRefresh)
	}
}
