package rest

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/log"
)

// Middleware is a function that wraps an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain combines multiple middleware into a single middleware.
func Chain(middlewares ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		for i := len(middlewares) - 1; i >= 0; i-- {
			next = middlewares[i](next)
		}
		return next
	}
}

// Logger returns a middleware that logs HTTP requests.
func Logger(logger log.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Create a response wrapper to capture the status code
			wrapper := &responseWrapper{ResponseWriter: w, status: http.StatusOK}

			// Process the request
			next.ServeHTTP(wrapper, r)

			// Log the request
			duration := time.Since(start)
			logger.Info("HTTP Request",
				log.Str("method", r.Method),
				log.Str("path", r.URL.Path),
				log.Int("status", wrapper.status),
				log.Duration("duration", duration),
				log.Str("remote_addr", r.RemoteAddr),
				log.Str("user_agent", r.UserAgent()),
			)
		})
	}
}

// APIKey returns a middleware that checks for a valid API key.
func APIKey(apiKeys []string, logger log.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth check if API keys are empty
			if len(apiKeys) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			// Get the Authorization header
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				logger.Warn("Missing Authorization header")
				http.Error(w, "Unauthorized: Missing API key", http.StatusUnauthorized)
				return
			}

			// Check that it's a Bearer token
			if !strings.HasPrefix(authHeader, "Bearer ") {
				logger.Warn("Invalid Authorization header format")
				http.Error(w, "Unauthorized: Invalid Authorization format", http.StatusUnauthorized)
				return
			}

			// Extract the API key
			apiKey := strings.TrimPrefix(authHeader, "Bearer ")

			// Validate the API key
			valid := false
			for _, key := range apiKeys {
				if apiKey == key {
					valid = true
					break
				}
			}

			if !valid {
				logger.Warn("Invalid API key")
				http.Error(w, "Unauthorized: Invalid API key", http.StatusUnauthorized)
				return
			}

			// API key is valid, proceed
			next.ServeHTTP(w, r)
		})
	}
}

// CORS returns a middleware that adds CORS headers to the response.
func CORS() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Set CORS headers
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

			// Handle preflight requests
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}

			// Process the request
			next.ServeHTTP(w, r)
		})
	}
}

// Recovery returns a middleware that recovers from panics.
func Recovery(logger log.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					logger.Error("Panic recovered", log.Any("error", err))
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// Timeout returns a middleware that adds a timeout to the request context.
func Timeout(timeout time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()

			r = r.WithContext(ctx)
			next.ServeHTTP(w, r)
		})
	}
}

// responseWrapper is a wrapper for http.ResponseWriter that captures the status code.
type responseWrapper struct {
	http.ResponseWriter
	status int
}

// WriteHeader captures the status code and passes it to the wrapped ResponseWriter.
func (rw *responseWrapper) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}
