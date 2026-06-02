package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
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

type handoffPostBody struct {
	Token string `json:"token"`
}

type handoffGetBody struct {
	Token string `json:"token"`
}

// handoffHandler serves POST (store) and GET (claim) for /v1/ui/handoff/{code}.
// POST body: {"token":"..."}. GET response: {"token":"..."}.
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
			var body handoffPostBody
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
				http.Error(w, "invalid body", http.StatusBadRequest)
				return
			}
			if strings.TrimSpace(body.Token) == "" {
				http.Error(w, "empty token", http.StatusBadRequest)
				return
			}
			s.handoff.put(code, body.Token)
			uiHandoffResult("created")
			w.WriteHeader(http.StatusNoContent)

		case http.MethodGet:
			token, result := s.handoff.take(code)
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
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(handoffGetBody{Token: token})

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}
