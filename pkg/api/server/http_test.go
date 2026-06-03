package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/api/service"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc"
)

// uiTestEnv bundles a running httptest server in front of the dashboard HTTP
// handler, plus tokens for subjects with different policy sets.
type uiTestEnv struct {
	ts        *httptest.Server
	rootToken string // root policy (has ui:access via "*")
	roToken   string // readonly policy (has explicit ui:access)
	castToken string // cast policy (no ui:access)
}

// newUITestEnv builds a minimal but real server: the actual auth + rbac
// interceptor chain wrapping AuthService and HealthService, fronted by the
// vanguard transcoder and the rest of the dashboard mux. No orchestrator is
// started, so this exercises the HTTP/transcoder plumbing in isolation.
func newUITestEnv(t *testing.T) *uiTestEnv {
	t.Helper()
	ctx := context.Background()

	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := SeedBuiltinPolicies(ctx, st); err != nil {
		t.Fatalf("seed policies: %v", err)
	}

	logger := log.GetDefaultLogger()
	s, err := New(WithAuth(nil), WithStore(st), WithLogger(logger),
		WithUI(UIOptions{Enabled: true, Path: "/ui", HandoffEnabled: true, HandoffTTL: 50 * time.Millisecond}))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	// Build a gRPC server with the production interceptor chain and the two
	// services this test needs.
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			s.authUnaryInterceptor(),
			s.rbacUnaryInterceptor(),
		)),
	)
	generated.RegisterAuthServiceServer(grpcServer, service.NewAuthService(st, logger))
	generated.RegisterHealthServiceServer(grpcServer, service.NewHealthService(st, s.runnerManager, logger))
	s.grpcServer = grpcServer

	handler, err := s.buildHTTPHandler()
	if err != nil {
		t.Fatalf("build http handler: %v", err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	mkSubject := func(name, policy string) string {
		u := &types.User{Name: name, ID: name, Policies: []string{policy}}
		if err := st.Create(ctx, types.ResourceTypeUser, "system", name, u); err != nil {
			t.Fatalf("create user %s: %v", name, err)
		}
		_, secret, err := repos.NewTokenRepo(st).IssueStatic(ctx, name+"-tok", name, "user", "", 0)
		if err != nil {
			t.Fatalf("issue token for %s: %v", name, err)
		}
		return secret
	}

	return &uiTestEnv{
		ts:        ts,
		rootToken: mkSubject("rooty", "root"),
		roToken:   mkSubject("reader", "readonly"),
		castToken: mkSubject("ci", "cast"),
	}
}

// rpc issues a Connect unary-over-JSON POST to the transcoder and returns the
// status code and decoded JSON body.
func (e *uiTestEnv) rpc(t *testing.T, method, token string, body any) (int, map[string]any) {
	t.Helper()
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+"/grpc/"+method, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded
}

// TestTranscoder_VersionRoundTrips proves the core spike: an HTTP Connect-JSON
// request transcodes through vanguard into the gRPC stack and back. It uses the
// auth-exempt GetServerVersion so it works without a token.
func TestTranscoder_VersionRoundTrips(t *testing.T) {
	e := newUITestEnv(t)
	code, body := e.rpc(t, "rune.api.HealthService/GetServerVersion", "", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("expected 200 from GetServerVersion, got %d (body %v)", code, body)
	}
}

// TestTranscoder_WhoAmIWithToken proves the auth interceptor runs through the
// transcoder: a valid bearer token yields the caller's subject.
func TestTranscoder_WhoAmIWithToken(t *testing.T) {
	e := newUITestEnv(t)
	code, body := e.rpc(t, "rune.api.AuthService/WhoAmI", e.rootToken, map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("expected 200 WhoAmI, got %d (body %v)", code, body)
	}
	if body["subjectId"] != "rooty" && body["subject_id"] != "rooty" {
		t.Fatalf("expected subject rooty, got body %v", body)
	}
}

// TestTranscoder_WhoAmINoToken proves the auth interceptor rejects unauthenticated
// calls even through the transcoder (no token => not 200).
func TestTranscoder_WhoAmINoToken(t *testing.T) {
	e := newUITestEnv(t)
	code, _ := e.rpc(t, "rune.api.AuthService/WhoAmI", "", map[string]any{})
	if code == http.StatusOK {
		t.Fatalf("expected non-200 for unauthenticated WhoAmI, got 200")
	}
}

// TestUIAccess_CastDenied proves the ui:access gate: a token on the `cast`
// policy (no ui:access) gets 403 at the HTTP layer before reaching gRPC.
func TestUIAccess_CastDenied(t *testing.T) {
	e := newUITestEnv(t)
	code, _ := e.rpc(t, "rune.api.AuthService/WhoAmI", e.castToken, map[string]any{})
	if code != http.StatusForbidden {
		t.Fatalf("expected 403 for cast token (no ui:access), got %d", code)
	}
}

// TestUIAccess_ReadonlyAllowed proves readonly's explicit ui:access rule works.
func TestUIAccess_ReadonlyAllowed(t *testing.T) {
	e := newUITestEnv(t)
	code, _ := e.rpc(t, "rune.api.AuthService/WhoAmI", e.roToken, map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("expected 200 for readonly token (has ui:access), got %d", code)
	}
}

// TestUIAccess_NoToken proves the gate rejects missing credentials with 401.
func TestUIAccess_NoToken(t *testing.T) {
	e := newUITestEnv(t)
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/grpc/rune.api.AuthService/WhoAmI", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	e := newUITestEnv(t)
	resp, err := e.ts.Client().Get(e.ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz expected 200, got %d", resp.StatusCode)
	}
}

func TestUIPlaceholderAndSecurityHeaders(t *testing.T) {
	e := newUITestEnv(t)
	resp, err := e.ts.Client().Get(e.ts.URL + "/ui/")
	if err != nil {
		t.Fatalf("get /ui/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/ui/ expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP missing frame-ancestors 'none': %q", csp)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "UI not built") {
		t.Errorf("placeholder body missing 'UI not built' marker")
	}
}

func TestUITrailingSlashRedirect(t *testing.T) {
	e := newUITestEnv(t)
	client := e.ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(e.ts.URL + "/ui")
	if err != nil {
		t.Fatalf("get /ui: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("/ui expected 302, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Errorf("redirect Location = %q, want /ui/", loc)
	}
}

func TestRootRedirect(t *testing.T) {
	e := newUITestEnv(t)
	client := e.ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("/ expected 302, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Errorf("root redirect Location = %q, want /ui/", loc)
	}
}

// TestUIMountPathNormalisation covers the path normalisation, including the
// "/" edge case that would otherwise collide with the root handler and panic
// http.ServeMux at startup.
func TestUIMountPathNormalisation(t *testing.T) {
	cases := map[string]string{
		"":         "/ui",
		"/ui":      "/ui",
		"ui":       "/ui",
		"/ui/":     "/ui",
		"/console": "/console",
		"/":        "/ui", // must fall back, not become ""
		"  /ui  ":  "/ui",
		"/a/b/":    "/a/b",
	}
	for in, want := range cases {
		s := &APIServer{options: &Options{UI: UIOptions{Path: in}}}
		if got := s.uiMountPath(); got != want {
			t.Errorf("uiMountPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildHTTPHandlerRootPathDoesNotPanic guards the regression directly: a
// configured path of "/" must not panic buildHTTPHandler via duplicate "/"
// ServeMux registration.
func TestBuildHTTPHandlerRootPathDoesNotPanic(t *testing.T) {
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open: %v", err)
	}
	s, err := New(WithStore(st), WithUI(UIOptions{Enabled: true, Path: "/"}))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	s.grpcServer = grpc.NewServer()
	generated.RegisterHealthServiceServer(s.grpcServer, service.NewHealthService(st, s.runnerManager, log.GetDefaultLogger()))
	if _, err := s.buildHTTPHandler(); err != nil {
		t.Fatalf("buildHTTPHandler with path=/ returned error: %v", err)
	}
}

// TestUILoginPage serves the minimal sign-in page (CLI handoff claim landing).
func TestUILoginPage(t *testing.T) {
	e := newUITestEnv(t)
	resp, err := e.ts.Client().Get(e.ts.URL + "/ui/login?handoff=abc")
	if err != nil {
		t.Fatalf("get login page: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login page expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("login page should be HTML, got %q", ct)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("login page must forbid framing")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "/v1/ui/handoff/") {
		t.Fatal("login page should claim the handoff code")
	}
}

// TestHandoffEndpoint exercises the RUNE-201 cookie handoff: an authenticated
// POST mints a browser-scoped grant; GET claims it once and delivers an HttpOnly
// refresh cookie; the code is single-use and TTL-bounded.
func TestHandoffEndpoint(t *testing.T) {
	e := newUITestEnv(t)
	client := e.ts.Client()

	// POST authenticated with the CLI's bearer (no body).
	post := func(code, bearer string) int {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/v1/ui/handoff/"+code, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("post handoff: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// GET claims the code; returns status + the rune_refresh cookie value (if any).
	get := func(code string) (int, string) {
		resp, err := client.Get(e.ts.URL + "/v1/ui/handoff/" + code)
		if err != nil {
			t.Fatalf("get handoff: %v", err)
		}
		defer resp.Body.Close()
		var cookie string
		for _, c := range resp.Cookies() {
			if c.Name == refreshCookieName {
				cookie = c.Value
			}
		}
		return resp.StatusCode, cookie
	}

	// Unauthenticated POST is rejected.
	if c := post("noauth", ""); c != http.StatusUnauthorized {
		t.Fatalf("unauthenticated post expected 401, got %d", c)
	}

	if c := post("abc123", e.rootToken); c != http.StatusNoContent {
		t.Fatalf("post expected 204, got %d", c)
	}
	code, cookie := get("abc123")
	if code != http.StatusNoContent {
		t.Fatalf("first get expected 204, got %d", code)
	}
	if cookie == "" {
		t.Fatal("claim did not set a refresh cookie")
	}
	// The delivered cookie is a usable refresh grant: exchange it at the refresh
	// endpoint for an access token (the browser's first-token flow).
	{
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+refreshMountPath, nil)
		req.AddCookie(&http.Cookie{Name: refreshCookieName, Value: cookie})
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("refresh with handoff cookie: %v", err)
		}
		sc := resp.StatusCode
		resp.Body.Close()
		if sc != http.StatusOK {
			t.Fatalf("handoff cookie should refresh to an access token, got %d", sc)
		}
	}

	// Single use: second get is 404.
	if code, _ := get("abc123"); code != http.StatusNotFound {
		t.Fatalf("second get expected 404, got %d", code)
	}
	// TTL expiry (store TTL is 50ms in the test env).
	post("expiring", e.rootToken)
	time.Sleep(80 * time.Millisecond)
	if code, _ := get("expiring"); code != http.StatusNotFound {
		t.Fatalf("expired get expected 404, got %d", code)
	}
}
