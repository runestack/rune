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

// accessTokenGCInterval is how often expired access tokens are swept. Chosen
// ≥ the access TTL so the table never accumulates more than ~one TTL of dead
// rows per active grant.
const accessTokenGCInterval = 10 * time.Minute

// startAccessTokenGC launches the background sweep that evicts expired
// access-kind tokens (RUNE-201). It runs until the server's shutdown channel
// closes and is registered on the wait group so Stop() drains it.
func (s *APIServer) startAccessTokenGC() {
	if s.store == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(accessTokenGCInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.shutdownCh:
				return
			case <-ticker.C:
				repo := repos.NewTokenRepo(s.store)
				n, err := repo.DeleteExpiredAccessTokens(context.Background(), time.Now())
				if err != nil {
					s.logger.Warn("access-token GC sweep failed", log.Err(err))
					continue
				}
				if n > 0 {
					s.logger.Debug("access-token GC sweep", log.Int("deleted", n))
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

		out, result := s.refresh.Rotate(r.Context(), secret)
		switch result {
		case session.ResultOK:
			if cookieMode {
				http.SetCookie(w, s.refreshCookie(out.Refresh, s.refresh.RefreshTTL))
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
				http.SetCookie(w, s.refreshCookie("", -time.Hour)) // clear
			}
			http.Error(w, "refresh token reuse detected; session revoked", http.StatusUnauthorized)

		case session.ResultInvalid:
			if cookieMode {
				http.SetCookie(w, s.refreshCookie("", -time.Hour)) // clear
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

// refreshCookie builds the rotated/cleared refresh cookie. Secure is set under
// TLS; SameSite=Strict + Path-scoping are the CSRF defense (RUNE-201 CSRF §).
func (s *APIServer) refreshCookie(value string, ttl time.Duration) *http.Cookie {
	c := &http.Cookie{
		Name:     refreshCookieName,
		Value:    value,
		Path:     refreshMountPath,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.options.EnableTLS,
	}
	if ttl < 0 {
		c.MaxAge = -1 // delete now
	} else if ttl > 0 {
		c.MaxAge = int(ttl / time.Second)
	}
	return c
}
