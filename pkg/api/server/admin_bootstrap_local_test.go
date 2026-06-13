package server

import (
	"context"
	"net"
	"testing"

	"github.com/runestack/rune/pkg/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// TestAdminBootstrap_LoopbackOnly verifies the bootstrap lockdown: AdminBootstrap
// mints an auth-exempt cluster-admin token, so the admin interceptor must allow
// it ONLY from a loopback peer, and fail closed when the peer is absent
// (e.g. an in-process dashboard-transcoder call) or remote — regardless of
// auth.allow_remote_admin. The supported path is `rune admin bootstrap` on the
// server.
func TestAdminBootstrap_LoopbackOnly(t *testing.T) {
	st := store.NewTestStore()
	_ = st.Open("")
	s, err := New(WithAuth(nil), WithStore(st))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	const method = "/rune.api.AdminService/AdminBootstrap"
	info := &grpc.UnaryServerInfo{FullMethod: method}
	called := false
	h := func(ctx context.Context, req interface{}) (interface{}, error) { called = true; return nil, nil }

	call := func(ctx context.Context) error {
		called = false
		_, e := s.adminUnaryInterceptor()(ctx, nil, info, h)
		return e
	}
	withPeer := func(addr string) context.Context {
		return peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP(addr), Port: 5555}})
	}

	// Loopback peer → allowed (handler runs).
	if err := call(withPeer("127.0.0.1")); err != nil {
		t.Fatalf("loopback bootstrap should be allowed, got %v", err)
	}
	if !called {
		t.Fatal("handler should have run for loopback caller")
	}

	// Remote peer → denied.
	if err := call(withPeer("203.0.113.5")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("remote bootstrap should be PermissionDenied, got %v", err)
	}

	// No peer (in-process / unknown locality) → denied (fail closed).
	if err := call(context.Background()); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("peerless bootstrap should be PermissionDenied (fail closed), got %v", err)
	}
}
