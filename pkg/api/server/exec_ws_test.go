package server

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/api/service"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/orchestrator"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// execWSTestEnv wires a real gRPC ExecService (backed by FakeOrchestrator) and
// the WebSocket bridge in front of it, exactly as runed does — so the bridge's
// self-dial + the gRPC auth/rbac interceptors are all exercised.
type execWSTestEnv struct {
	wsURL     string
	rootToken string // root: ui:access + exec:exec
	roToken   string // readonly: ui:access but NOT exec:exec
	castToken string // cast: no ui:access
}

func newExecWSTestEnv(t *testing.T) *execWSTestEnv {
	t.Helper()
	ctx := context.Background()

	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := SeedBuiltinPolicies(ctx, st); err != nil {
		t.Fatalf("seed policies: %v", err)
	}

	fo := orchestrator.NewFakeOrchestrator()
	fo.ExecStdout = []byte("sample command output")
	fo.ExecStderr = []byte("sample error output")
	fo.ExecExitCode = 0
	inst := &types.Instance{
		ID: "instance123", Namespace: "default", Name: "test-instance",
		ServiceID: "service123", ServiceName: "test-service",
		ContainerID: "container123", Status: types.InstanceStatusRunning,
	}
	if err := st.Create(ctx, types.ResourceTypeInstance, "default", "instance123", inst); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	fo.AddInstance(inst)

	logger := log.GetDefaultLogger()
	s, err := New(WithAuth(nil), WithStore(st), WithLogger(logger),
		WithUI(UIOptions{Enabled: true, Path: "/ui"}))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	// Real gRPC server with the production stream interceptors, on a random
	// loopback port; point the bridge's self-dial at it.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.options.GRPCAddr = lis.Addr().String()
	gsrv := grpc.NewServer(grpc.ChainStreamInterceptor(
		s.authStreamInterceptor(),
		s.rbacStreamInterceptor(),
	))
	generated.RegisterExecServiceServer(gsrv, service.NewExecService(logger, fo))
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)

	// WS bridge fronted by the metrics middleware (so the statusRecorder
	// Hijack delegation is exercised too).
	ts := httptest.NewServer(s.uiMetricsMiddleware(s.execWSHandler()))
	t.Cleanup(ts.Close)

	mk := func(name, policy string) string {
		u := &types.User{Name: name, ID: name, Policies: []string{policy}}
		if err := st.Create(ctx, types.ResourceTypeUser, "system", name, u); err != nil {
			t.Fatalf("user %s: %v", name, err)
		}
		_, secret, err := repos.NewTokenRepo(st).Issue(ctx, name+"-tok", name, "user", "", 0)
		if err != nil {
			t.Fatalf("token %s: %v", name, err)
		}
		return secret
	}

	return &execWSTestEnv{
		wsURL:     "ws" + strings.TrimPrefix(ts.URL, "http"),
		rootToken: mk("execroot", "root"),
		roToken:   mk("execreader", "readonly"),
		castToken: mk("execci", "cast"),
	}
}

func (e *execWSTestEnv) dial(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	subs := []string{execSubprotocol}
	if token != "" {
		subs = []string{execBearerSubproto + token, execSubprotocol}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, e.wsURL, &websocket.DialOptions{Subprotocols: subs})
}

func initFrame(t *testing.T) []byte {
	t.Helper()
	b, err := proto.Marshal(&generated.ExecRequest{
		Request: &generated.ExecRequest_Init{Init: &generated.ExecInitRequest{
			Target:    &generated.ExecInitRequest_InstanceId{InstanceId: "instance123"},
			Namespace: "default",
			Command:   []string{"ls", "-la"},
		}},
	})
	if err != nil {
		t.Fatalf("marshal init: %v", err)
	}
	return b
}

// TestExecWS_RoundTrip proves the full browser exec path: WS in, init frame,
// proxied through the gRPC ExecService, stdout + exit streamed back as proto
// frames over the socket.
func TestExecWS_RoundTrip(t *testing.T) {
	e := newExecWSTestEnv(t)
	conn, _, err := e.dial(t, e.rootToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if conn.Subprotocol() != execSubprotocol {
		t.Fatalf("negotiated subprotocol = %q, want %q", conn.Subprotocol(), execSubprotocol)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageBinary, initFrame(t)); err != nil {
		t.Fatalf("write init: %v", err)
	}

	var gotStdout, gotExit bool
	for i := 0; i < 20 && !(gotStdout && gotExit); i++ {
		typ, data, rerr := conn.Read(ctx)
		if rerr != nil {
			break // stream closed by server
		}
		if typ != websocket.MessageBinary {
			continue
		}
		var resp generated.ExecResponse
		if err := proto.Unmarshal(data, &resp); err != nil {
			t.Fatalf("unmarshal resp: %v", err)
		}
		switch r := resp.Response.(type) {
		case *generated.ExecResponse_Stdout:
			if bytes.Contains(r.Stdout, []byte("sample command output")) {
				gotStdout = true
			}
		case *generated.ExecResponse_Exit:
			gotExit = true
		}
	}
	if !gotStdout {
		t.Error("did not receive expected stdout over the WS bridge")
	}
	if !gotExit {
		t.Error("did not receive an exit frame over the WS bridge")
	}
}

// TestExecWS_NoToken proves the bridge rejects a tokenless upgrade.
func TestExecWS_NoToken(t *testing.T) {
	e := newExecWSTestEnv(t)
	conn, resp, err := e.dial(t, "")
	if err == nil {
		conn.CloseNow()
		t.Fatal("expected dial to fail without a token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		got := 0
		if resp != nil {
			got = resp.StatusCode
		}
		t.Fatalf("expected 401, got %d", got)
	}
}

// TestExecWS_CastDenied proves ui:access gating: a cast token (no ui:access) is
// rejected at the WS handler before any exec.
func TestExecWS_CastDenied(t *testing.T) {
	e := newExecWSTestEnv(t)
	conn, resp, err := e.dial(t, e.castToken)
	if err == nil {
		conn.CloseNow()
		t.Fatal("expected dial to fail for cast token")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		got := 0
		if resp != nil {
			got = resp.StatusCode
		}
		t.Fatalf("expected 403, got %d", got)
	}
}

// TestExecWS_ReadonlyExecDeniedServerSide proves exec:exec is enforced by the
// gRPC interceptor: a readonly token passes the ui:access gate (upgrade
// succeeds) but the exec stream is denied server-side, so no stdout arrives.
func TestExecWS_ReadonlyExecDeniedServerSide(t *testing.T) {
	e := newExecWSTestEnv(t)
	conn, _, err := e.dial(t, e.roToken)
	if err != nil {
		t.Fatalf("dial should succeed (readonly has ui:access): %v", err)
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageBinary, initFrame(t))

	for {
		typ, data, rerr := conn.Read(ctx)
		if rerr != nil {
			return // stream closed/denied — expected
		}
		if typ != websocket.MessageBinary {
			continue
		}
		var resp generated.ExecResponse
		if err := proto.Unmarshal(data, &resp); err == nil {
			if r, ok := resp.Response.(*generated.ExecResponse_Stdout); ok &&
				bytes.Contains(r.Stdout, []byte("sample command output")) {
				t.Fatal("readonly token must not receive exec stdout (exec:exec should be denied)")
			}
		}
	}
}
