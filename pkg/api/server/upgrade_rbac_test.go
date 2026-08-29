package server

import (
	"context"
	"net"
	"testing"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const upgradeMethod = "/rune.api.AdminService/UpgradeServer"

// The remote-callability of UpgradeServer rests on methodToAction and
// methodToResource agreeing: the dedicated ("server","upgrade") action, and
// a resource that is not "admin" (which would re-arm the localhost-only
// gate). A half-updated mapping is the asymmetry that fails open.
func TestUpgradeServerRBAC(t *testing.T) {
	if r, v := methodToAction(upgradeMethod); r != "server" || v != "upgrade" {
		t.Fatalf("methodToAction = (%q,%q), want (server,upgrade)", r, v)
	}
	// Assert the two agree, not merely that one is not "admin": a
	// half-applied rename satisfies the negative and is exactly the drift
	// this test exists to catch — it has already happened once.
	if got, want := methodToResource(upgradeMethod), "server"; got != want {
		t.Fatalf("methodToResource = %q, want %q (must match methodToAction)", got, want)
	}
	if r, _ := methodToAction(upgradeMethod); methodToResource(upgradeMethod) != r {
		t.Fatalf("methodToAction and methodToResource disagree: %q vs %q", r, methodToResource(upgradeMethod))
	}
	if methodToResource(upgradeMethod) == "admin" {
		t.Fatal("methodToResource must not be \"admin\" — that re-arms the localhost-only gate")
	}
	// And it must never become public: unauthenticated binary replacement.
	if isPublicMethod(upgradeMethod) {
		t.Fatal("UpgradeServer must never be in publicMethodSuffixes")
	}
}

// TestUpgradeServerRBAC_BuiltinGrants asserts only the *:* builtins (root,
// admin) hold system:upgrade; the fixed-verb builtins are denied.
func TestUpgradeServerRBAC_BuiltinGrants(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestStore()
	_ = st.Open("")
	_ = SeedBuiltinPolicies(ctx, st)

	for name, policy := range map[string]string{
		"rootsub": "root", "adminsub": "admin",
		"rwsub": "readwrite", "rosub": "readonly", "castsub": "cast",
	} {
		u := &types.User{Name: name, ID: name, Policies: []string{policy}}
		_ = st.Create(ctx, types.ResourceTypeUser, "system", name, u)
	}

	s, err := New(WithAuth(nil), WithStore(st))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	call := func(subject string) error {
		cctx := context.WithValue(context.Background(), authCtxKey, &AuthInfo{SubjectID: subject})
		info := &grpc.UnaryServerInfo{FullMethod: upgradeMethod}
		h := func(ctx context.Context, req interface{}) (interface{}, error) { return nil, nil }
		_, err := s.rbacUnaryInterceptor()(cctx, nil, info, h)
		return err
	}

	for _, allowed := range []string{"rootsub", "adminsub"} {
		if err := call(allowed); err != nil {
			t.Fatalf("%s must be allowed: %v", allowed, err)
		}
	}
	for _, denied := range []string{"rwsub", "rosub", "castsub"} {
		if err := call(denied); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s must be denied, got %v", denied, err)
		}
	}
}

// TestUpgradeServerSkipsAdminLocalhostGate proves the resource mapping
// exempts UpgradeServer from the admin localhost-only interceptor (a
// non-loopback peer with allow_remote_admin unset) — remote upgrade
// without opening the whole admin surface is the point of the mapping.
func TestUpgradeServerSkipsAdminLocalhostGate(t *testing.T) {
	st := store.NewTestStore()
	_ = st.Open("")
	s, err := New(WithAuth(nil), WithStore(st))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	interceptor := s.adminUnaryInterceptor()

	handlerRan := false
	h := func(ctx context.Context, req interface{}) (interface{}, error) {
		handlerRan = true
		return nil, nil
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 5555}})
	if _, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: upgradeMethod}, h); err != nil {
		t.Fatalf("UpgradeServer from a remote peer must pass the admin gate (RBAC still applies): %v", err)
	}
	if !handlerRan {
		t.Fatal("handler did not run")
	}

	// Control: a plain admin method from the same remote peer is refused.
	if _, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/rune.api.AdminService/PolicyList"}, h); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("plain admin method from remote peer must be denied, got %v", err)
	}
}
