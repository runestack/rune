package server

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Test minimal RBAC policy on unary interceptor
func TestRBACUnaryInterceptor(t *testing.T) {
	ctx := context.Background()

	st := store.NewTestStore()
	_ = st.Open("")
	_ = SeedBuiltinPolicies(ctx, st)
	// subject used in test
	u := &types.User{Name: "sub", ID: "sub", Policies: []string{"root"}}
	_ = st.Create(ctx, types.ResourceTypeUser, "system", "sub", u)

	s, err := New(WithAuth(nil), WithStore(st))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	// helper
	call := func(hasSubject bool, method string) error {
		ctx := context.Background()
		if hasSubject {
			ctx = context.WithValue(ctx, authCtxKey, &AuthInfo{SubjectID: "sub"})
		}
		info := &grpc.UnaryServerInfo{FullMethod: method}
		h := func(ctx context.Context, req interface{}) (interface{}, error) { return nil, nil }
		_, err := s.rbacUnaryInterceptor()(ctx, nil, info, h)
		return err
	}

	// without subject should be denied
	err = call(false, "/rune.api.ServiceService/GetService")
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected permission denied without subject, got %v", err)
	}
	// with subject allowed via policy
	if err := call(true, "/rune.api.ServiceService/CreateService"); err != nil {
		t.Fatalf("expected allow with subject: %v", err)
	}
}

// Test RBAC on stream interceptor for logs (read) vs exec (write)
func TestRBACStreamInterceptor(t *testing.T) {
	ctx := context.Background()

	st := store.NewTestStore()
	_ = st.Open("")
	_ = SeedBuiltinPolicies(ctx, st)
	u := &types.User{Name: "sub", ID: "sub", Policies: []string{"root"}}
	_ = st.Create(ctx, types.ResourceTypeUser, "system", "sub", u)

	s, err := New(WithAuth(nil), WithStore(st))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	call := func(hasSubject bool, method string) error {
		ctx := context.Background()
		if hasSubject {
			ctx = context.WithValue(ctx, authCtxKey, &AuthInfo{SubjectID: "sub"})
		}
		ss := &fakeServerStream{ctx: ctx}
		info := &grpc.StreamServerInfo{FullMethod: method}
		h := func(srv interface{}, stream grpc.ServerStream) error { return nil }
		return s.rbacStreamInterceptor()(nil, ss, info, h)
	}

	// without subject should be denied
	err = call(false, "/rune.api.LogService/StreamLogs")
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected permission denied without subject, got %v", err)
	}
	// with subject allowed via policy
	if err := call(true, "/rune.api.ExecService/StreamExec"); err != nil {
		t.Fatalf("expected allow with subject: %v", err)
	}
}

// TestRBACSetDefaultStorageClass exercises the per-request RBAC requirement
// added by extraRBACRequirements (RUNE-073): setting Default:true on a
// StorageClass requires the additional `set-default` verb on top of the
// standard create/update verb. The built-in `readwrite` policy grants
// create/update but NOT set-default, so it must be denied. `root` (verb "*")
// must still be allowed.
func TestRBACSetDefaultStorageClass(t *testing.T) {
	ctx := context.Background()

	st := store.NewTestStore()
	_ = st.Open("")
	_ = SeedBuiltinPolicies(ctx, st)

	rwUser := &types.User{Name: "rw", ID: "rw", Policies: []string{"readwrite"}}
	_ = st.Create(ctx, types.ResourceTypeUser, "system", "rw", rwUser)
	rootUser := &types.User{Name: "root-user", ID: "root-user", Policies: []string{"root"}}
	_ = st.Create(ctx, types.ResourceTypeUser, "system", "root-user", rootUser)

	s, err := New(WithAuth(nil), WithStore(st))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	call := func(subjectID string, defaultFlag bool) error {
		c := context.WithValue(context.Background(), authCtxKey, &AuthInfo{SubjectID: subjectID})
		req := &generated.CreateStorageClassRequest{
			StorageClass: &generated.StorageClass{Name: "fast", Driver: "local", Default: defaultFlag},
		}
		info := &grpc.UnaryServerInfo{FullMethod: "/rune.api.StorageClassService/CreateStorageClass"}
		h := func(ctx context.Context, req interface{}) (interface{}, error) { return nil, nil }
		_, err := s.rbacUnaryInterceptor()(c, req, info, h)
		return err
	}

	// readwrite token: create without Default is allowed.
	if err := call("rw", false); err != nil {
		t.Fatalf("readwrite create (default=false) expected allow, got %v", err)
	}
	// readwrite token: create WITH Default:true is denied (no set-default verb).
	if err := call("rw", true); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("readwrite create (default=true) expected PermissionDenied, got %v", err)
	}
	// root token: both allowed.
	if err := call("root-user", true); err != nil {
		t.Fatalf("root create (default=true) expected allow, got %v", err)
	}
}

// TestRBACPrivilegedService exercises the per-request RBAC requirement
// for CreateService when the payload requests admin-only security
// knobs (privileged=true or seccompProfile=unconfined, on either the
// main container or any init step). The built-in `readwrite` policy
// grants services.create but NOT services.privileged, so the gated
// payloads must be denied. `root` (verb "*") must still be allowed.
func TestRBACPrivilegedService(t *testing.T) {
	ctx := context.Background()

	st := store.NewTestStore()
	_ = st.Open("")
	_ = SeedBuiltinPolicies(ctx, st)

	rwUser := &types.User{Name: "rw", ID: "rw", Policies: []string{"readwrite"}}
	_ = st.Create(ctx, types.ResourceTypeUser, "system", "rw", rwUser)
	rootUser := &types.User{Name: "root-user", ID: "root-user", Policies: []string{"root"}}
	_ = st.Create(ctx, types.ResourceTypeUser, "system", "root-user", rootUser)

	s, err := New(WithAuth(nil), WithStore(st))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	call := func(subjectID string, svc *generated.Service) error {
		c := context.WithValue(context.Background(), authCtxKey, &AuthInfo{SubjectID: subjectID})
		req := &generated.CreateServiceRequest{Service: svc}
		info := &grpc.UnaryServerInfo{FullMethod: "/rune.api.ServiceService/CreateService"}
		h := func(ctx context.Context, req interface{}) (interface{}, error) { return nil, nil }
		_, err := s.rbacUnaryInterceptor()(c, req, info, h)
		return err
	}

	plain := &generated.Service{Name: "api", Namespace: "app", Image: "nginx"}
	privMain := &generated.Service{
		Name: "api", Namespace: "app", Image: "nginx",
		SecurityContext: &generated.SecurityContext{Privileged: true},
	}
	unconfinedMain := &generated.Service{
		Name: "api", Namespace: "app", Image: "nginx",
		SecurityContext: &generated.SecurityContext{
			SeccompProfile: &generated.SeccompProfile{Type: "unconfined"},
		},
	}
	privInit := &generated.Service{
		Name: "tigerbeetle", Namespace: "shared", Image: "tb",
		InitSteps: []*generated.InitStep{{
			Name: "format", Image: "tb", Command: "/format",
			SecurityContext: &generated.SecurityContext{
				SeccompProfile: &generated.SeccompProfile{Type: "unconfined"},
			},
		}},
	}

	// readwrite token: a plain create is allowed.
	if err := call("rw", plain); err != nil {
		t.Fatalf("readwrite create (plain) expected allow, got %v", err)
	}
	// readwrite token: privileged main is denied.
	if err := call("rw", privMain); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("readwrite create (privileged main) expected PermissionDenied, got %v", err)
	}
	// readwrite token: seccomp=unconfined on main is denied.
	if err := call("rw", unconfinedMain); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("readwrite create (unconfined main) expected PermissionDenied, got %v", err)
	}
	// readwrite token: seccomp=unconfined on an init step is denied.
	if err := call("rw", privInit); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("readwrite create (unconfined init step) expected PermissionDenied, got %v", err)
	}
	// root token: all four are allowed.
	for name, svc := range map[string]*generated.Service{"plain": plain, "privMain": privMain, "unconfinedMain": unconfinedMain, "privInit": privInit} {
		if err := call("root-user", svc); err != nil {
			t.Fatalf("root create (%s) expected allow, got %v", name, err)
		}
	}

	// Regression for Bug E (Propeller v0.0.1-dev.41 follow-up): the
	// gate must trigger on the k8s-style PascalCase "Unconfined", not
	// just our lowercase form. Pre-v0.0.1-dev.42 the case mismatch
	// silently let the request through (and the runner silently
	// dropped the seccomp opt downstream).
	unconfinedPascal := &generated.Service{
		Name: "api", Namespace: "app", Image: "nginx",
		SecurityContext: &generated.SecurityContext{
			SeccompProfile: &generated.SeccompProfile{Type: "Unconfined"},
		},
	}
	if err := call("rw", unconfinedPascal); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("readwrite create (Unconfined PascalCase main) expected PermissionDenied, got %v", err)
	}
}

type fakeServerStream struct{ ctx context.Context }

func (f *fakeServerStream) SetHeader(md metadata.MD) error  { return nil }
func (f *fakeServerStream) SendHeader(md metadata.MD) error { return nil }
func (f *fakeServerStream) SetTrailer(md metadata.MD)       {}
func (f *fakeServerStream) Context() context.Context        { return f.ctx }
func (f *fakeServerStream) SendMsg(m interface{}) error     { return nil }
func (f *fakeServerStream) RecvMsg(m interface{}) error     { return nil }
