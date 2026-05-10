package repos

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
)

// AuditRepo provides append-only persistence and query access for audit events.
//
// Events are stored under the reserved namespace "system" with names of the
// form "<rfc3339nano-ts>-<uuid>" so that the natural list order from the
// underlying store is roughly chronological. The id field on the event is the
// authoritative identifier; the storage name is an implementation detail.
type AuditRepo struct {
	core store.Store
}

const auditNamespace = "system"

// NewAuditRepo constructs an audit repository backed by the given store.
func NewAuditRepo(core store.Store) *AuditRepo {
	return &AuditRepo{core: core}
}

// Append persists an audit event. ID and Timestamp are populated if unset.
// Append never errors out the caller's request path on failure; callers should
// log the error and continue. Returning the error here lets callers decide.
func (r *AuditRepo) Append(ctx context.Context, evt *types.AuditEvent) error {
	if evt == nil {
		return fmt.Errorf("nil audit event")
	}
	if evt.ID == "" {
		evt.ID = uuid.NewString()
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	// Storage key chosen to make natural lexicographic order roughly
	// chronological. Real ordering is enforced by sorting on List.
	name := fmt.Sprintf("%s-%s", evt.Timestamp.UTC().Format("20060102T150405.000000000"), evt.ID)
	return r.core.Create(ctx, types.ResourceTypeAuditEvent, auditNamespace, name, evt)
}

// AuditFilter narrows a List query. Zero-value fields are ignored.
type AuditFilter struct {
	Resource    string    // exact match on Resource
	ResourceRef string    // exact match on ResourceRef ("ns/name")
	Namespace   string    // exact match on Namespace
	Actor       string    // exact match on Actor
	Action      string    // exact match on Action
	Since       time.Time // events with Timestamp >= Since
	Until       time.Time // events with Timestamp < Until (zero = no bound)
}

// List returns audit events matching the filter, newest first, capped at limit.
// A limit of 0 or less applies a default cap of 200.
func (r *AuditRepo) List(ctx context.Context, f AuditFilter, limit int) ([]*types.AuditEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	var raw []types.AuditEvent
	if err := r.core.List(ctx, types.ResourceTypeAuditEvent, auditNamespace, &raw); err != nil {
		return nil, err
	}
	// Filter
	out := make([]*types.AuditEvent, 0, len(raw))
	for i := range raw {
		evt := &raw[i]
		if f.Resource != "" && evt.Resource != f.Resource {
			continue
		}
		if f.ResourceRef != "" && evt.ResourceRef != f.ResourceRef {
			continue
		}
		if f.Namespace != "" && evt.Namespace != f.Namespace {
			continue
		}
		if f.Actor != "" && evt.Actor != f.Actor {
			continue
		}
		if f.Action != "" && evt.Action != f.Action {
			continue
		}
		if !f.Since.IsZero() && evt.Timestamp.Before(f.Since) {
			continue
		}
		if !f.Until.IsZero() && !evt.Timestamp.Before(f.Until) {
			continue
		}
		out = append(out, evt)
	}
	// Newest first.
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// MakeRef returns the canonical "<namespace>/<name>" string used as ResourceRef
// in audit events. Returns just the name when namespace is empty.
func MakeAuditRef(namespace, name string) string {
	if namespace == "" {
		return name
	}
	if name == "" {
		return ""
	}
	return strings.TrimSpace(namespace) + "/" + strings.TrimSpace(name)
}
