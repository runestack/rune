package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/session"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
)

// newRefreshTestServer builds an APIServer wired with a session manager — enough
// to exercise the HTTP cookie endpoint in isolation. Rotation-core behaviour is
// tested in pkg/api/session; here we cover the HTTP transport (cookie vs body).
func newRefreshTestServer(t *testing.T) (*APIServer, store.Store) {
	t.Helper()
	st := store.NewTestStore()
	if err := st.Open(""); err != nil {
		t.Fatalf("open store: %v", err)
	}
	s, err := New(WithStore(st), WithLogger(log.GetDefaultLogger()))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	s.refresh = session.New(st, s.logger)
	return s, st
}

// Browser mode: refresh token rides an HttpOnly cookie; the response rotates it
// via Set-Cookie and returns only the access token in the body.
func TestRefreshHandler_CookieRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, st := newRefreshTestServer(t)
	_, secret, err := repos.NewTokenRepo(st).IssueRefreshGrant(ctx, "chrome", "alice", "user", time.Hour)
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}

	ts := httptest.NewServer(s.refreshHandler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// seed the cookie as login/handoff would, then refresh.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+refreshMountPath, nil)
	req.AddCookie(&http.Cookie{Name: refreshCookieName, Value: secret})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body refreshRespBody
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body.AccessToken == "" {
		t.Fatal("no access token in body")
	}
	if body.RefreshToken != "" {
		t.Fatal("SECURITY: refresh token leaked into cookie-mode response body")
	}

	// Cookie should have rotated to a new value and auto-attach next call.
	var rotated string
	jarURL, _ := url.Parse(ts.URL + refreshMountPath)
	for _, c := range jar.Cookies(jarURL) {
		if c.Name == refreshCookieName {
			rotated = c.Value
		}
	}
	if rotated == "" || rotated == secret {
		t.Fatalf("refresh cookie did not rotate, got %q", rotated)
	}

	// The access token must be a usable request bearer; the rotated refresh must not.
	if _, err := repos.NewTokenRepo(st).FindRequestBearer(ctx, body.AccessToken); err != nil {
		t.Fatalf("minted access token should be a valid bearer: %v", err)
	}
	if _, err := repos.NewTokenRepo(st).FindRequestBearer(ctx, rotated); err == nil {
		t.Fatal("SECURITY: rotated refresh token accepted as a bearer")
	}
}

// CLI mode: refresh token in the body; both tokens returned in the body, no cookie.
func TestRefreshHandler_BodyMode(t *testing.T) {
	ctx := context.Background()
	s, st := newRefreshTestServer(t)
	_, secret, err := repos.NewTokenRepo(st).IssueRefreshGrant(ctx, "cli", "alice", "user", time.Hour)
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}

	ts := httptest.NewServer(s.refreshHandler())
	defer ts.Close()

	payload, _ := json.Marshal(refreshReqBody{RefreshToken: secret})
	resp, err := http.Post(ts.URL+refreshMountPath, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("body mode must not set a cookie")
	}
	var body refreshRespBody
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.AccessToken == "" || body.RefreshToken == "" {
		t.Fatalf("body mode must return both tokens, got %+v", body)
	}
	_ = ctx
}

func TestRefreshHandler_InvalidToken(t *testing.T) {
	s, _ := newRefreshTestServer(t)
	ts := httptest.NewServer(s.refreshHandler())
	defer ts.Close()

	payload, _ := json.Marshal(refreshReqBody{RefreshToken: "rune_not-a-real-token"})
	resp, err := http.Post(ts.URL+refreshMountPath, "application/json", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid token should be 401, got %d", resp.StatusCode)
	}
}
