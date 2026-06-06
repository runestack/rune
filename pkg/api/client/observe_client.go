package client

import (
	"context"
	"io"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
)

// ObserveClient wraps the native observability (RuneSight) ObserveService:
// LogQL history queries, the backend capability handshake, and ingest. Used by
// `rune logs --since/--until` (history) and the dashboard.
type ObserveClient struct {
	client *Client
	logger log.Logger
	svc    generated.ObserveServiceClient
}

// NewObserveClient creates a new observe client.
func NewObserveClient(client *Client) *ObserveClient {
	return &ObserveClient{
		client: client,
		logger: client.logger.WithComponent("observe-client"),
		svc:    generated.NewObserveServiceClient(client.conn),
	}
}

// Capabilities reports the backend handshake. Capabilities.Enabled is false
// when observability is disabled; callers should fall back to the live log
// stream in that case.
func (c *ObserveClient) Capabilities(ctx context.Context) (*generated.ObserveCapabilities, error) {
	return c.svc.GetCapabilities(ctx, &generated.CapabilitiesRequest{})
}

// ExecuteQuery runs a LogQL query over the persisted store and returns the
// server stream of QueryResults. The caller drives Recv until io.EOF.
func (c *ObserveClient) ExecuteQuery(ctx context.Context, q *generated.ObserveQuery) (generated.ObserveService_ExecuteClient, error) {
	c.logger.Debug("Executing observe query", log.Str("logql", q.GetLogql()))
	return c.svc.Execute(ctx, q)
}

// QueryLogs is a convenience wrapper that runs a LogQL query and collects the
// returned log rows into a slice. It is meant for the non-follow `rune logs`
// history path; metric samples are ignored. start/end are RFC3339-formatted
// internally.
func (c *ObserveClient) QueryLogs(ctx context.Context, logql, namespace string, start, end time.Time, limit int, forward bool) ([]*generated.LogRow, error) {
	req := &generated.ObserveQuery{
		Logql:     logql,
		Namespace: namespace,
		Limit:     uint32(maxInt(limit, 0)), //nolint:gosec // limit bounded non-negative
		Forward:   forward,
	}
	if !start.IsZero() {
		req.Start = start.UTC().Format(time.RFC3339)
	}
	if !end.IsZero() {
		req.End = end.UTC().Format(time.RFC3339)
	}

	stream, err := c.ExecuteQuery(ctx, req)
	if err != nil {
		return nil, err
	}
	var rows []*generated.LogRow
	for {
		res, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return rows, err
		}
		if row := res.GetRow(); row != nil {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// PushLogs ingests a batch of records (the multi-node forwarder transport).
func (c *ObserveClient) PushLogs(ctx context.Context, records []*generated.LogRecord) (uint32, error) {
	resp, err := c.svc.PushLogs(ctx, &generated.PushLogsRequest{Records: records})
	if err != nil {
		return 0, err
	}
	return resp.GetAccepted(), nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
