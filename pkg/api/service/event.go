package service

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/events"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// EventService implements the gRPC EventService (RUNE-126 Phase 2).
// Thin wrapper around the EventLog — the recorder owns storage and
// fold semantics; this service is just transport.
type EventService struct {
	generated.UnimplementedEventServiceServer

	log    events.EventLog
	logger log.Logger
}

// NewEventService constructs an EventService over the given EventLog.
func NewEventService(eventLog events.EventLog, logger log.Logger) *EventService {
	return &EventService{
		log:    eventLog,
		logger: logger.WithComponent("event-service"),
	}
}

// ListEvents returns the most-recent events for the requested target.
// For="<kind>/<name>" narrows to one resource; empty For lists across
// the namespace (currently implemented as "all recent events" — the
// EventLog has no namespace-wide indexed view yet, so we fall back to
// the global Seq cursor and filter by namespace in process). Limit
// defaults to 20 when zero.
func (s *EventService) ListEvents(ctx context.Context, req *generated.ListEventsRequest) (*generated.ListEventsResponse, error) {
	if s.log == nil {
		return nil, status.Error(codes.Unavailable, "event log not configured on this server")
	}
	limit := int(req.Limit)
	if limit <= 0 {
		limit = 20
	}

	var (
		out []types.Event
		err error
	)
	if req.For != "" {
		kind, name, ok := splitKindName(req.For)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "for must be <kind>/<name>, got %q", req.For)
		}
		canonKind := canonicalEventKind(kind)
		// Node events are cluster-scoped: the record they describe lives
		// under an empty-namespace key, so honouring the caller's
		// namespace here returns nothing on any install with a default
		// namespace configured — a write-only event log (RUNE-301
		// §12.3). Forced HERE rather than in the CLI so the namespace the
		// caller sent still reaches RBAC: a namespace-pinned grant is
		// allowed to read node events (they are hardware facts, not
		// tenant data) and `rune describe node` already shows them.
		ns := req.Namespace
		if canonKind == "Node" {
			ns = ""
		}
		out, err = s.log.ListByResource(ctx, ns, canonKind, name, limit)
	} else {
		// Namespace-wide view: scan the cursor index and filter. Cheap
		// for typical TTL windows (~1h of state-transition events).
		var all []types.Event
		all, err = s.log.ListSince(ctx, 0, 1000)
		if err == nil {
			// Newest first, filter by namespace.
			for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
				if req.Namespace != "" && all[i].Namespace != req.Namespace {
					continue
				}
				out = append(out, all[i])
			}
		}
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list events: %v", err)
	}
	return &generated.ListEventsResponse{
		Events: protoEvents(out),
		Status: &generated.Status{Code: int32(codes.OK)},
	}, nil
}

// canonicalEventKind normalises a user-supplied kind ("instance",
// "INSTANCE", "Instance" …) to the form controllers store under
// (capitalised: "Instance" / "Service" / "Volume" / "Node"). Anything
// else is title-cased best-effort so previously-unknown kinds still
// match if a future emitter writes them with a different case.
func canonicalEventKind(k string) string {
	switch strings.ToLower(k) {
	case "instance", "instances", "inst":
		return "Instance"
	case "service", "services", "svc":
		return "Service"
	case "volume", "volumes", "vol":
		return "Volume"
	case "node", "nodes":
		return "Node"
	}
	if k == "" {
		return ""
	}
	return strings.ToUpper(k[:1]) + strings.ToLower(k[1:])
}

// splitKindName parses "<kind>/<name>" — kind/name must be non-empty
// and contain no further slashes (resource names are DNS-1123).
func splitKindName(s string) (kind, name string, ok bool) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Contains(parts[1], "/") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// protoEvents converts a slice of domain Events to wire form.
func protoEvents(in []types.Event) []*generated.Event {
	if len(in) == 0 {
		return nil
	}
	out := make([]*generated.Event, 0, len(in))
	for _, e := range in {
		count := e.Count
		if count < 0 {
			count = 0
		} else if count > math.MaxInt32 {
			count = math.MaxInt32
		}
		out = append(out, &generated.Event{
			Id:        e.ID,
			Seq:       e.Seq,
			Namespace: e.Namespace,
			Kind:      e.Kind,
			Name:      e.Name,
			Uid:       e.UID,
			Level:     string(e.Level),
			Reason:    e.Reason,
			Message:   e.Message,
			FirstSeen: e.FirstSeen.UTC().Format(time.RFC3339),
			LastSeen:  e.LastSeen.UTC().Format(time.RFC3339),
			Count:     int32(count), //nolint:gosec // bounded above
		})
	}
	return out
}
