package service

import (
	"context"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AuditService exposes the append-only security audit trail. RBAC is enforced
// upstream by the standard rbac interceptor — see methodToAction in
// pkg/api/server/utils.go (resource: "audit").
type AuditService struct {
	generated.UnimplementedAuditServiceServer
	repo   *repos.AuditRepo
	logger log.Logger
}

func NewAuditService(coreStore store.Store, logger log.Logger) *AuditService {
	return &AuditService{repo: repos.NewAuditRepo(coreStore), logger: logger}
}

func (s *AuditService) ListAuditEvents(ctx context.Context, req *generated.ListAuditEventsRequest) (*generated.ListAuditEventsResponse, error) {
	if req == nil {
		req = &generated.ListAuditEventsRequest{}
	}
	filter := repos.AuditFilter{
		Resource:    req.Resource,
		ResourceRef: req.ResourceRef,
		Namespace:   req.Namespace,
		Actor:       req.Actor,
		Action:      req.Action,
	}
	if req.Since != "" {
		t, err := time.Parse(time.RFC3339, req.Since)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid since timestamp: %v", err)
		}
		filter.Since = t
	}
	if req.Until != "" {
		t, err := time.Parse(time.RFC3339, req.Until)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid until timestamp: %v", err)
		}
		filter.Until = t
	}
	limit := int(req.Limit)
	events, err := s.repo.List(ctx, filter, limit)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list audit events: %v", err)
	}
	out := make([]*generated.AuditEvent, 0, len(events))
	for _, e := range events {
		out = append(out, auditToProto(e))
	}
	return &generated.ListAuditEventsResponse{Events: out, Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

func auditToProto(e *types.AuditEvent) *generated.AuditEvent {
	if e == nil {
		return nil
	}
	return &generated.AuditEvent{
		Id:          e.ID,
		Timestamp:   e.Timestamp.UTC().Format(time.RFC3339Nano),
		Actor:       e.Actor,
		Action:      e.Action,
		Resource:    e.Resource,
		ResourceRef: e.ResourceRef,
		Namespace:   e.Namespace,
		Outcome:     string(e.Outcome),
		Message:     e.Message,
		Metadata:    e.Metadata,
	}
}
