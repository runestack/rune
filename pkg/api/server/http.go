package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/vanguard"
	"connectrpc.com/vanguard/vanguardgrpc"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store/repos"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc/encoding"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	grpcMountPrefix    = "/grpc"
	handoffMountPrefix = "/v1/ui/handoff/"
)

// jsonCodecOnce guards the process-global registration of the gRPC "json"
// codec that vanguard uses to pass Connect-JSON and REST traffic through
// without a re-encode. Registration is global state, so do it at most once.
var jsonCodecOnce sync.Once

func registerJSONCodec() {
	jsonCodecOnce.Do(func() {
		encoding.RegisterCodec(vanguardgrpc.NewCodec(&vanguard.JSONCodec{
			MarshalOptions:   protojson.MarshalOptions{EmitUnpopulated: true},
			UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
		}))
	})
}

// startHTTPServer brings up the dashboard HTTP serving layer on HTTPAddr
// (RUNE-200). It is only called when UI.Enabled is true. The server hosts:
//
//   - /grpc/...            vanguard transcoder → existing gRPC stack
//   - /v1/ui/handoff/{code} CLI token handoff
//   - <ui.Path>/...        embedded SPA (placeholder in Phase 1)
//   - /healthz, /readyz    liveness/readiness
//   - /                    redirect to the dashboard
//
// The gRPC server must already be constructed (startGRPCServer) so its
// registered services can be wrapped. Traffic through /grpc dispatches into
// grpcServer.ServeHTTP, so every existing interceptor (auth, admin, rbac,
// recovery, validator) runs unchanged.
func (s *APIServer) startHTTPServer() error {
	mux, err := s.buildHTTPHandler()
	if err != nil {
		return err
	}

	// Transport selection for HTTP/2 (needed by the transcoder's bidi streams):
	//   - TLS on:  serve the plain handler; net/http negotiates h2 via ALPN.
	//   - TLS off: wrap in h2c so cleartext HTTP/2 works in dev and behind a
	//              TLS-terminating ingress that re-origins h2c to runed.
	// Wrapping in h2c *and* serving TLS is contradictory, so the two paths are
	// kept distinct.
	handler := mux
	if !s.options.EnableTLS {
		handler = h2c.NewHandler(mux, &http2.Server{})
	}

	addr := s.httpBindAddr()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	s.httpServer = &http.Server{
		Handler: handler,
		// Bound the header-read window to blunt Slowloris-style stalls. No
		// overall ReadTimeout/WriteTimeout: the transcoder carries long-lived
		// log/exec streams that must not be cut off mid-flight.
		ReadHeaderTimeout: 10 * time.Second,
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.logger.Info("Starting dashboard HTTP server",
			log.Str("address", addr), log.Str("ui_path", s.uiMountPath()))
		var serveErr error
		if s.options.EnableTLS {
			serveErr = s.httpServer.ServeTLS(lis, s.options.TLSCertFile, s.options.TLSKeyFile)
		} else {
			serveErr = s.httpServer.Serve(lis)
		}
		if serveErr != nil && serveErr != http.ErrServerClosed {
			s.logger.Error("dashboard HTTP server error", log.Err(serveErr))
		}
	}()

	return nil
}

// buildHTTPHandler assembles the dashboard mux: the vanguard transcoder under
// /grpc, the CLI handoff endpoint, the embedded SPA, health probes, and the
// root redirect — wrapped in the request-metrics middleware. Split out from
// startHTTPServer so tests can exercise it via httptest without binding a port
// or starting the orchestrator. Requires s.grpcServer to be populated and
// s.handoff to be initialised.
func (s *APIServer) buildHTTPHandler() (http.Handler, error) {
	registerJSONCodec()

	if s.handoff == nil {
		s.handoff = newHandoffStore(s.options.UI.HandoffTTL)
	}
	s.ensureRefreshManager()

	mux := http.NewServeMux()

	// gRPC-Web / Connect / REST transcoder over the existing gRPC services.
	transcoder, err := vanguardgrpc.NewTranscoder(s.grpcServer)
	if err != nil {
		return nil, fmt.Errorf("build gRPC-Web transcoder: %w", err)
	}
	grpcHandler := http.StripPrefix(grpcMountPrefix, transcoder)
	mux.Handle(grpcMountPrefix+"/", s.uiAccessMiddleware(grpcHandler))

	// CLI token handoff.
	mux.Handle(handoffMountPrefix, s.handoffHandler())

	// RUNE-201 session refresh (plain HTTP so Set-Cookie is emitted natively).
	mux.HandleFunc(refreshMountPath, s.refreshHandler())

	// Exec WebSocket bridge (RUNE-200C §3) — browser exec, since bidi gRPC
	// isn't browser-callable.
	mux.Handle(execWSPath, s.execWSHandler())

	// Embedded dashboard.
	mountPath := s.uiMountPath()
	uiHandler, err := s.uiHandler(mountPath)
	if err != nil {
		return nil, fmt.Errorf("build UI handler: %w", err)
	}
	mux.Handle(mountPath+"/", uiHandler)
	// Minimal sign-in page handling the CLI→browser handoff (RUNE-201). Exact
	// path, so it takes precedence over the SPA subtree handler above. The full
	// SPA will later own this route.
	mux.HandleFunc(mountPath+"/login", s.uiLoginHandler(mountPath))
	// /ui (no trailing slash) → /ui/
	mux.HandleFunc(mountPath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, mountPath+"/", http.StatusFound)
	})

	// Liveness / readiness.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	// Root redirect.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, mountPath+"/", http.StatusFound)
	})

	return s.uiMetricsMiddleware(mux), nil
}

// uiMountPath returns the configured dashboard mount path, normalised to a
// leading-slash, no-trailing-slash form (e.g. "/ui"). A path that normalises to
// empty (unset, or "/") falls back to "/ui": mounting the SPA at the root would
// collide with the "/" redirect handler and panic http.ServeMux at startup.
func (s *APIServer) uiMountPath() string {
	p := strings.TrimSpace(s.options.UI.Path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimRight(p, "/")
	if p == "" {
		p = "/ui"
	}
	return p
}

// httpBindAddr applies the TLS posture: when RequireTLS is set but TLS is off,
// the server refuses to expose bearer-token traffic on a public interface and
// binds loopback only.
func (s *APIServer) httpBindAddr() string {
	addr := s.options.HTTPAddr
	if s.options.UI.RequireTLS && !s.options.EnableTLS {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			// addr was just ":7861" or similar; SplitHostPort fails on a
			// bare ":port" only if malformed — fall back to the raw value.
			port = strings.TrimPrefix(addr, ":")
		}
		loopback := "127.0.0.1:" + port
		if addr != loopback {
			s.logger.Warn("UI require_tls is set but TLS is disabled; binding dashboard to loopback only",
				log.Str("requested", addr), log.Str("bound", loopback))
		}
		return loopback
	}
	return addr
}

// uiAccessMiddleware gates /grpc traffic on the ui:access permission so an
// operator can mint a token that explicitly cannot drive the dashboard (e.g. a
// CI service account on the `cast` policy). The per-RPC policy interceptor
// inside the gRPC stack remains authoritative for the actual operation; this
// is an additional, dashboard-specific gate.
func (s *APIServer) uiAccessMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.options.EnableAuth || isPublicGRPCPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		token := bearerFromRequest(r)
		if token == "" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		tok, err := repos.NewTokenRepo(s.store).FindRequestBearer(r.Context(), token)
		if err != nil {
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		allowed, err := s.evaluatePolicies(r.Context(), tok.SubjectID, "ui", "access", "")
		if err != nil {
			http.Error(w, "authorization error", http.StatusInternalServerError)
			return
		}
		if !allowed {
			http.Error(w, "ui:access denied", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// publicMethodSuffixes are the gRPC methods reachable without a request bearer:
// either intentionally public (version probe, bootstrap) or self-authenticating
// on their own payload (RUNE-201 refresh / enrollment redeem). Defined once and
// consulted by the auth interceptor, the rbac interceptor, AND the dashboard
// transcoder gate (isPublicGRPCPath), so the exemption set cannot drift between
// the native-gRPC and HTTP transports. HasSuffix matches both the exact gRPC
// full-method ("/rune.api.X/M") and the transcoder path ("/grpc/rune.api.X/M").
var publicMethodSuffixes = []string{
	"/rune.api.HealthService/GetServerVersion",
	"/rune.api.AdminService/AdminBootstrap",
	"/rune.api.AuthService/Refresh",
	"/rune.api.AuthService/RedeemEnrollment",
}

func isPublicMethod(fullMethod string) bool {
	for _, suffix := range publicMethodSuffixes {
		if strings.HasSuffix(fullMethod, suffix) {
			return true
		}
	}
	return false
}

// isPublicGRPCPath reports whether an RPC reached over the transcoder bypasses
// the dashboard ui:access gate, mirroring the gRPC interceptor exemptions.
func isPublicGRPCPath(p string) bool { return isPublicMethod(p) }

// bearerFromRequest extracts a bearer token from the Authorization header.
func bearerFromRequest(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// statusRecorder captures the response status code for metrics.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates to the underlying ResponseWriter so WebSocket upgrades
// (the exec bridge) work even though this wrapper sits in the metrics
// middleware. Without this, the wrapper would mask the http.Hijacker the
// WebSocket library needs.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := s.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// uiMetricsMiddleware records request counts and active stream gauge. It is a
// method so route classification honours the configured (possibly custom) UI
// mount path rather than a hardcoded "/ui".
func (s *APIServer) uiMetricsMiddleware(next http.Handler) http.Handler {
	uiPrefix := s.uiMountPath()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := routeClass(r.URL.Path, uiPrefix)
		if route == "grpc" {
			uiActiveStreams.Inc()
			defer uiActiveStreams.Dec()
		}
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		uiRequestsTotal.WithLabelValues(route, strconv.Itoa(rec.code)).Inc()
	})
}

// routeClass buckets a path into a low-cardinality label for metrics.
func routeClass(p, uiPrefix string) string {
	switch {
	case strings.HasPrefix(p, grpcMountPrefix+"/"):
		return "grpc"
	case strings.HasPrefix(p, handoffMountPrefix):
		return "handoff"
	case p == refreshMountPath:
		return "refresh"
	case p == execWSPath:
		return "exec"
	case p == "/healthz" || p == "/readyz":
		return "health"
	case p == uiPrefix || strings.HasPrefix(p, uiPrefix+"/"):
		return "ui"
	default:
		return "other"
	}
}
