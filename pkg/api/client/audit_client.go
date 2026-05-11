package client

import (
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
)

// AuditClient provides access to the server-side audit log.
type AuditClient struct {
	client *Client
	logger log.Logger
	svc    generated.AuditServiceClient
}

// NewAuditClient creates a new audit client.
func NewAuditClient(client *Client) *AuditClient {
	return &AuditClient{
		client: client,
		logger: client.logger.WithComponent("audit-client"),
		svc:    generated.NewAuditServiceClient(client.conn),
	}
}

// AuditListOptions narrows a ListAuditEvents call. Zero-value fields are
// ignored. Limit of 0 lets the server apply its default cap.
type AuditListOptions struct {
	Resource    string
	ResourceRef string
	Namespace   string
	Actor       string
	Action      string
	Since       time.Time
	Until       time.Time
	Limit       int
}

// ListAuditEvents returns audit events matching the filter, newest first.
func (a *AuditClient) ListAuditEvents(opts AuditListOptions) ([]*types.AuditEvent, error) {
	req := &generated.ListAuditEventsRequest{
		Resource:    opts.Resource,
		ResourceRef: opts.ResourceRef,
		Namespace:   opts.Namespace,
		Actor:       opts.Actor,
		Action:      opts.Action,
		Limit:       int32(opts.Limit), //nolint:gosec // G115: API limit bounded by caller; proto field is int32
	}
	if !opts.Since.IsZero() {
		req.Since = opts.Since.UTC().Format(time.RFC3339)
	}
	if !opts.Until.IsZero() {
		req.Until = opts.Until.UTC().Format(time.RFC3339)
	}

	ctx, cancel := a.client.Context()
	defer cancel()

	resp, err := a.svc.ListAuditEvents(ctx, req)
	if err != nil {
		a.logger.Error("Failed to list audit events", log.Err(err))
		return nil, convertGRPCError("list audit events", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}

	out := make([]*types.AuditEvent, 0, len(resp.Events))
	for _, e := range resp.Events {
		out = append(out, protoToAudit(e))
	}
	return out, nil
}

func protoToAudit(p *generated.AuditEvent) *types.AuditEvent {
	if p == nil {
		return nil
	}
	ts, _ := time.Parse(time.RFC3339Nano, p.Timestamp)
	return &types.AuditEvent{
		ID:          p.Id,
		Timestamp:   ts,
		Actor:       p.Actor,
		Action:      p.Action,
		Resource:    p.Resource,
		ResourceRef: p.ResourceRef,
		Namespace:   p.Namespace,
		Outcome:     types.AuditOutcome(p.Outcome),
		Message:     p.Message,
		Metadata:    p.Metadata,
	}
}
