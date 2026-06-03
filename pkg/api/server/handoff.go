package server

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/store/repos"
)

// handoffStore is the short-lived, in-memory token store backing the
// `rune ui login` CLI handoff flow (RUNE-200). The CLI POSTs its bearer token
// against a one-time code; the browser SPA GETs it exactly once to seed its
// session. Codes are single-use and expire after a TTL. Nothing is persisted —
// a daemon restart drops all pending handoffs, which is the desired behaviour.
type handoffStore struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[string]handoffEntry
}

type handoffEntry struct {
	token  string
	expiry time.Time
}

func newHandoffStore(ttl time.Duration) *handoffStore {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &handoffStore{
		ttl: ttl,
		now: time.Now,
		m:   make(map[string]handoffEntry),
	}
}

// put stores a token under code, overwriting any prior entry, with a fresh TTL.
func (h *handoffStore) put(code, token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gcLocked()
	h.m[code] = handoffEntry{token: token, expiry: h.now().Add(h.ttl)}
}

// takeResult distinguishes the outcome of a handoff claim for metrics/logging.
type takeResult int

const (
	takeClaimed takeResult = iota
	takeExpired
	takeNotFound
)

// take returns the token for code and removes it (single use). The result
// distinguishes a successful claim from an expired vs. unknown code so callers
// can report them separately. Note: gc only runs on put(), so an expired entry
// is still present here and reported as takeExpired rather than takeNotFound.
func (h *handoffStore) take(code string) (string, takeResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.m[code]
	if !ok {
		return "", takeNotFound
	}
	delete(h.m, code)
	if h.now().After(e.expiry) {
		return "", takeExpired
	}
	return e.token, takeClaimed
}

// gcLocked evicts expired entries. Caller must hold h.mu.
func (h *handoffStore) gcLocked() {
	now := h.now()
	for k, e := range h.m {
		if now.After(e.expiry) {
			delete(h.m, k)
		}
	}
}

// handoffCodeFromPath extracts {code} from /v1/ui/handoff/{code}.
func handoffCodeFromPath(p string) string {
	const prefix = "/v1/ui/handoff/"
	if !strings.HasPrefix(p, prefix) {
		return ""
	}
	return strings.Trim(strings.TrimPrefix(p, prefix), "/")
}

// handoffHandler serves the CLI→browser session handoff for
// /v1/ui/handoff/{code} (RUNE-200 + RUNE-201 cookie flow).
//
//   - POST is authenticated with the CLI's bearer (Authorization header). The
//     server mints a fresh, browser-scoped refresh grant for that subject and
//     parks the grant secret under the one-time code. The browser-scoped grant
//     is independently revocable and never exposes the CLI's own credential.
//   - GET (from the browser) claims the code and delivers the grant as an
//     HttpOnly, SameSite=Strict refresh cookie — it never touches JS or
//     sessionStorage. The SPA then mints its first access token via
//     /v1/auth/refresh (the standard RUNE-201 browser flow).
func (s *APIServer) handoffHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.options.UI.HandoffEnabled {
			http.Error(w, "handoff disabled", http.StatusNotFound)
			return
		}
		code := handoffCodeFromPath(r.URL.Path)
		if code == "" || len(code) > 256 {
			http.Error(w, "invalid handoff code", http.StatusBadRequest)
			return
		}

		switch r.Method {
		case http.MethodPost:
			// Authenticate the CLI and mint a browser-scoped grant for its subject.
			token := bearerFromRequest(r)
			if token == "" {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			repo := repos.NewTokenRepo(s.store)
			tok, err := repo.FindRequestBearer(r.Context(), token)
			if err != nil {
				http.Error(w, "invalid bearer token", http.StatusUnauthorized)
				return
			}
			// Bound the grant with the sliding refresh window so an abandoned
			// handoff (minted but never claimed/used) eventually expires and is
			// GC'd, rather than living forever.
			_, grantSecret, err := repo.IssueRefreshGrant(r.Context(), "dashboard", tok.SubjectID, tok.SubjectType, s.ensureRefreshManager().RefreshTTL)
			if err != nil {
				http.Error(w, "failed to mint browser session", http.StatusInternalServerError)
				return
			}
			s.handoff.put(code, grantSecret)
			uiHandoffResult("created")
			w.WriteHeader(http.StatusNoContent)

		case http.MethodGet:
			grantSecret, result := s.handoff.take(code)
			switch result {
			case takeExpired:
				uiHandoffResult("expired")
				http.Error(w, "handoff expired", http.StatusNotFound)
				return
			case takeNotFound:
				uiHandoffResult("notfound")
				http.Error(w, "handoff not found", http.StatusNotFound)
				return
			}
			uiHandoffResult("claimed")
			// Deliver the grant as an HttpOnly refresh cookie; the SPA mints its
			// first access token via /v1/auth/refresh.
			http.SetCookie(w, s.refreshCookie(r, grantSecret, s.ensureRefreshManager().RefreshTTL))
			w.WriteHeader(http.StatusNoContent)

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}
