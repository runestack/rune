package client

import (
	"fmt"
	"math"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"google.golang.org/grpc/codes"
)

// EventClient wraps EventService (RUNE-126 Phase 2).
type EventClient struct {
	client *Client
	logger log.Logger
	svc    generated.EventServiceClient
}

// NewEventClient constructs an EventClient.
func NewEventClient(c *Client) *EventClient {
	return &EventClient{
		client: c,
		logger: c.logger.WithComponent("event-client"),
		svc:    generated.NewEventServiceClient(c.conn),
	}
}

// ListEvents returns recent events. forKind/forName narrow to a single
// resource when both are non-empty; pass "" for both to scan the
// namespace. limit <= 0 uses the server default.
func (e *EventClient) ListEvents(namespace, forKind, forName string, limit int) ([]*generated.Event, error) {
	ctx, cancel := e.client.Context()
	defer cancel()
	if limit < 0 {
		limit = 0
	} else if limit > math.MaxInt32 {
		limit = math.MaxInt32
	}
	req := &generated.ListEventsRequest{
		Namespace: namespace,
		Limit:     int32(limit), //nolint:gosec // bounded above
	}
	if forKind != "" && forName != "" {
		req.For = forKind + "/" + forName
	}
	resp, err := e.svc.ListEvents(ctx, req)
	if err != nil {
		return nil, convertGRPCError("list events", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return resp.Events, nil
}
