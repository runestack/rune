package ingress

import (
	"net"
	"sort"
	"strings"
	"sync"
)

// Route is the resolved upstream for a Host header at request time.
// The router does not own selection of a specific endpoint — that
// is the data plane's job. The router only resolves Host -> service.
type Route struct {
	// Host is the lowercased exact-match hostname.
	Host string
	// Namespace + Service identify the upstream service.
	Namespace string
	Service   string
	// Port is the service-level port to dial.
	Port int
	// Path is an optional path prefix; empty matches all.
	Path string
	// AllowCIDRs, when non-empty, restricts inbound connections to these
	// parsed source networks (matched against the real TCP peer). Empty =
	// no restriction. Parsed once at Apply time so the request path does no
	// parsing. See RUNE-0XX-Expose-Origin-Hardening-Design.md.
	AllowCIDRs []*net.IPNet
}

// Router is a host-keyed lookup for resolved upstream routes.
// Apply replaces the entire route table atomically. Match is wait-free
// for readers (RWMutex).
type Router struct {
	mu     sync.RWMutex
	byHost map[string][]Route // host -> routes sorted by len(Path) desc
}

// NewRouter returns an empty Router.
func NewRouter() *Router {
	return &Router{byHost: map[string][]Route{}}
}

// Apply replaces the route table with routes. The input is copied;
// the caller may reuse the slice. Hosts are normalized to lowercase
// and stripped of leading dots.
func (r *Router) Apply(routes []Route) {
	byHost := make(map[string][]Route, len(routes))
	for _, rt := range routes {
		h := normalizeHost(rt.Host)
		if h == "" || rt.Service == "" || rt.Port == 0 {
			continue
		}
		rt.Host = h
		byHost[h] = append(byHost[h], rt)
	}
	for h := range byHost {
		s := byHost[h]
		sort.Slice(s, func(i, j int) bool {
			return len(s[i].Path) > len(s[j].Path)
		})
		byHost[h] = s
	}
	r.mu.Lock()
	r.byHost = byHost
	r.mu.Unlock()
}

// Match returns the first route whose host (and optional path
// prefix) match. ok=false when no route matches.
func (r *Router) Match(host, path string) (Route, bool) {
	h := normalizeHost(host)
	r.mu.RLock()
	candidates := r.byHost[h]
	r.mu.RUnlock()
	for _, c := range candidates {
		if c.Path == "" || strings.HasPrefix(path, c.Path) {
			return c, true
		}
	}
	return Route{}, false
}

// PeerAllowed reports whether a connection from peerIP may proceed under
// this route's source-IP allowlist. An empty allowlist means "no
// restriction" (allow all) — never deny-all. A non-nil allowlist with no
// match denies. A nil/unparseable peerIP denies when an allowlist is set
// (fail closed: we can't prove the source is trusted).
func (rt Route) PeerAllowed(peerIP net.IP) bool {
	if len(rt.AllowCIDRs) == 0 {
		return true
	}
	if peerIP == nil {
		return false
	}
	for _, n := range rt.AllowCIDRs {
		if n != nil && n.Contains(peerIP) {
			return true
		}
	}
	return false
}

// Hosts returns a sorted snapshot of all hostnames in the table.
// Useful for the certificate loader to know what to pre-fetch.
func (r *Router) Hosts() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.byHost))
	for h := range r.byHost {
		out = append(out, h)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// normalizeHost strips a leading "." (some operators write
// ".example.com"), lowercases ASCII, and drops a port suffix.
func normalizeHost(h string) string {
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	h = strings.TrimPrefix(h, ".")
	return strings.ToLower(h)
}
