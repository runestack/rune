package client

import (
	"context"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
)

// LogClient provides methods for interacting with logs on the Rune API server.
type LogClient struct {
	client *Client
	logger log.Logger
	svc    generated.LogServiceClient
}

// NewLogClient creates a new log client.
func NewLogClient(client *Client) *LogClient {
	return &LogClient{
		client: client,
		logger: client.logger.WithComponent("log-client"),
		svc:    generated.NewLogServiceClient(client.conn),
	}
}

// StreamLogs creates a bidirectional stream for log streaming.
// This is a convenience method that wraps the underlying gRPC StreamLogs call.
func (c *LogClient) StreamLogs(ctx context.Context) (generated.LogService_StreamLogsClient, error) {
	c.logger.Debug("Opening log stream connection")
	return c.svc.StreamLogs(ctx)
}
