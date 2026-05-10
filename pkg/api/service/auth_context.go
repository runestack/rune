package service

import (
	"context"

	"github.com/runestack/rune/pkg/api/authctx"
)

// actorFromContext returns the audit-friendly actor identifier for the caller.
// Unauthenticated requests are recorded as "anonymous" rather than empty so
// that downstream queries can distinguish "no auth at all" from "authentication
// established but subject missing" (which is recorded as "unknown").
func actorFromContext(ctx context.Context) string {
	if ctx == nil {
		return "anonymous"
	}
	if subj := authctx.SubjectFrom(ctx); subj != "" {
		return subj
	}
	return "anonymous"
}
