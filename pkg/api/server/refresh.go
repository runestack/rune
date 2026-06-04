package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/session"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store/repos"
)

const (
	refreshCookieName = "rune_refresh"
	refreshMountPath  = "/v1/auth/refresh"
)

// ensureRefreshManager lazily builds the shared session manager, applying any
// SessionOptions TTL overrides (zero fields keep the session defaults).
func (s *APIServer) ensureRefreshManager() *session.Manager {
	if s.refresh != nil {
		return s.refresh
	}
	m := session.New(s.store, s.logger)
	if v := s.options.Session.AccessTTL; v > 0 {
		m.AccessTTL = v
	}
	if v := s.options.Session.RefreshTTL; v > 0 {
		m.RefreshTTL = v
	}
	if v := s.options.Session.GraceWindow; v > 0 {
		m.GraceWindow = v
	}
	s.refresh = m
	return m
}

// tokenGCInterval is how often expired tokens are swept. Bounded well below the
// refresh idle window so dead rows (expired access tokens, abandoned grants)
// don't accumulate for long; it need not relate to the access TTL since the
// sweep deletes by absolute expiry, not age.
const tokenGCInterval = 10 * time.Minute

// startTokenGC launches the background sweep that evicts expired tokens
// (RUNE-201). It runs until the server's shutdown channel closes and is
// registered on the wait group so Stop() drains it.
func (s *APIServer) startTokenGC() {
	if s.store == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(tokenGCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.shutdownCh:
				return
			case <-ticker.C:
				repo := repos.NewTokenRepo(s.store)
				n, err := repo.DeleteExpiredTokens(context.Background(), time.Now())
				if err != nil {
					s.logger.Warn("token GC sweep failed", log.Err(err))
					continue
				}
				if n > 0 {
					s.logger.Debug("token GC sweep", log.Int("deleted", n))
				}
			}
		}
	}()
}

// --- HTTP handler (browser cookie mode) -----------------------------------

type refreshReqBody struct {
	RefreshToken string `json:"refresh_token"`
}

type refreshRespBody struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"` // omitted in cookie mode
	ExpiresAt    int64  `json:"expires_at,omitempty"`    // unix seconds; 0 if no expiry
}

// refreshHandler serves POST /v1/auth/refresh. Two modes:
//   - Browser (cookie): presents rune_refresh as an HttpOnly cookie; the
//     response rotates it via Set-Cookie and returns only the access token in
//     the body (refresh never reaches JS).
//   - Body: presents {"refresh_token":...}; returns both tokens in the body.
//
// This is a plain HTTP endpoint (not a Connect RPC) specifically so Set-Cookie
// is emitted natively rather than relying on gRPC→HTTP metadata mapping through
// the transcoder (RUNE-201 transport decision; validated by spike). The CLI
// refreshes over the gRPC AuthService.Refresh RPC instead, so it does not depend
// on the (UI-gated) HTTP server being up.
func (s *APIServer) refreshHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		secret, cookieMode := s.presentedRefresh(r)
		if strings.TrimSpace(secret) == "" {
			http.Error(w, "missing refresh token", http.StatusBadRequest)
			return
		}

		mgr := s.ensureRefreshManager()
		out, result := mgr.Rotate(r.Context(), secret)
		switch result {
		case session.ResultOK:
			if cookieMode {
				http.SetCookie(w, s.refreshCookie(r, out.Refresh, mgr.RefreshTTL))
			}
			body := refreshRespBody{AccessToken: out.Access}
			if !cookieMode {
				body.RefreshToken = out.Refresh
			}
			if out.AccessExp != nil {
				body.ExpiresAt = out.AccessExp.Unix()
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)

		case session.ResultBreach:
			if cookieMode {
				http.SetCookie(w, s.refreshCookie(r, "", -time.Hour)) // clear
			}
			http.Error(w, "refresh token reuse detected; session revoked", http.StatusUnauthorized)

		case session.ResultInvalid:
			if cookieMode {
				http.SetCookie(w, s.refreshCookie(r, "", -time.Hour)) // clear
			}
			http.Error(w, "invalid refresh token", http.StatusUnauthorized)

		default:
			http.Error(w, "refresh failed", http.StatusInternalServerError)
		}
	}
}

// presentedRefresh extracts the refresh secret, preferring the cookie (browser)
// over a JSON body. The second return reports whether it came via cookie.
func (s *APIServer) presentedRefresh(r *http.Request) (string, bool) {
	if c, err := r.Cookie(refreshCookieName); err == nil && strings.TrimSpace(c.Value) != "" {
		return c.Value, true
	}
	var body refreshReqBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err == nil {
		return body.RefreshToken, false
	}
	return "", false
}

// refreshCookie builds the rotated/cleared refresh cookie. Secure tracks the
// externally-observed scheme (local TLS, a direct https request, or an
// X-Forwarded-Proto=https from a TLS-terminating proxy) so the grant is never
// emitted without Secure behind an ingress. SameSite=Strict + Path-scoping are
// the CSRF defense (RUNE-201 CSRF §).
func (s *APIServer) refreshCookie(r *http.Request, value string, ttl time.Duration) *http.Cookie {
	secure := s.options.EnableTLS || r.TLS != nil ||
		strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	c := &http.Cookie{
		Name:     refreshCookieName,
		Value:    value,
		Path:     refreshMountPath,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	}
	if ttl < 0 {
		c.MaxAge = -1 // delete now
	} else if ttl > 0 {
		c.MaxAge = int(ttl / time.Second)
	}
	return c
}
