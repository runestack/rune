package server

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// uiSecurityHeaders applies the dashboard's defensive headers (RUNE-200
// §Security Considerations): deny framing and lock the CSP down to same-origin
// assets. The Vite build emits hashed bundles with no inline scripts, so a
// strict default-src 'self' policy holds. connect-src allows the same origin
// so the SPA can reach /grpc and /v1.
func uiSecurityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head := w.Header()
		head.Set("X-Frame-Options", "DENY")
		head.Set("X-Content-Type-Options", "nosniff")
		head.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"font-src 'self'; "+
				"connect-src 'self'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'")
		h.ServeHTTP(w, r)
	})
}

// uiHandler serves the embedded single-page app under mountPath with SPA
// fallback: requests for real files (hashed assets) are served directly;
// anything else returns index.html so client-side routing works on deep links
// and page reloads.
func (s *APIServer) uiHandler(mountPath string) (http.Handler, error) {
	assets, err := uiAssetsFS()
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(assets))

	prefix := strings.TrimRight(mountPath, "/")

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip the mount prefix to get a path relative to the asset root.
		rel := strings.TrimPrefix(r.URL.Path, prefix)
		rel = strings.TrimPrefix(rel, "/")

		// The SPA shell is served directly (not via http.FileServer, which
		// redirects "/index.html" → "./" and loops). This covers the mount
		// root and any explicit index.html request.
		if rel == "" || rel == "index.html" {
			serveIndex(w, r, assets)
			return
		}

		// Real files (hashed bundles) are served from the embedded FS.
		if assetExists(assets, rel) {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/" + rel
			fileServer.ServeHTTP(w, r2)
			return
		}

		// Missing asset bundles 404 honestly; everything else falls back to
		// the SPA shell so client-side routing works on deep links/reloads.
		if strings.HasPrefix(rel, "assets/") {
			http.NotFound(w, r)
			return
		}
		serveIndex(w, r, assets)
	})

	return uiSecurityHeaders(h), nil
}

// assetExists reports whether name is a regular file in the embedded FS.
func assetExists(fsys fs.FS, name string) bool {
	name = path.Clean(name)
	if name == "." || strings.HasPrefix(name, "..") {
		return false
	}
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}
	return true
}

// serveIndex writes index.html from the embedded FS as the SPA shell.
func serveIndex(w http.ResponseWriter, _ *http.Request, fsys fs.FS) {
	f, err := fsys.Open("index.html")
	if err != nil {
		http.Error(w, "UI not available", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// SPA shell must not be cached aggressively; hashed assets carry their own
	// far-future caching via their content-hashed names.
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.Copy(w, f)
}
