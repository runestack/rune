// Package authctx provides a small shared context-key registry so that gRPC
// service handlers (under pkg/api/service) can read the authenticated subject
// stamped onto the context by the server-side auth interceptor (under
// pkg/api/server) without creating an import cycle.
//
// The server interceptor calls WithSubject; service handlers call SubjectFrom.
package authctx

import "context"

type ctxKey int

const subjectKey ctxKey = 1

// WithSubject returns a derived context that carries the given subject ID.
// The subject ID is the canonical identifier for the authenticated principal,
// typically the user ID stored on the verified token.
func WithSubject(ctx context.Context, subjectID string) context.Context {
	if subjectID == "" {
		return ctx
	}
	return context.WithValue(ctx, subjectKey, subjectID)
}

// SubjectFrom returns the subject ID previously attached via WithSubject, or
// the empty string if none is present.
func SubjectFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(subjectKey).(string)
	return v
}
