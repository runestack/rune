package client

import (
	"fmt"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"google.golang.org/grpc/codes"
)

// DescribeClient queries the one-shot resource diagnostics RPC (RUNE-126).
type DescribeClient struct {
	client *Client
	logger log.Logger
	dc     generated.DescribeServiceClient
}

// NewDescribeClient creates a new describe client.
func NewDescribeClient(client *Client) *DescribeClient {
	return &DescribeClient{
		client: client,
		logger: client.logger.WithComponent("describe-client"),
		dc:     generated.NewDescribeServiceClient(client.conn),
	}
}

// Describe assembles the diagnostic view for one resource. kind is the
// canonical resource kind ("instance" | "service" | "volume" | "node");
// namespace is ignored for cluster-scoped kinds.
func (d *DescribeClient) Describe(kind, name, namespace string) (*generated.DescribeResult, error) {
	ctx, cancel := d.client.Context()
	defer cancel()

	resp, err := d.dc.Describe(ctx, &generated.DescribeRequest{
		Kind:      kind,
		Name:      name,
		Namespace: namespace,
	})
	if err != nil {
		return nil, convertGRPCError("describe", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return resp.Result, nil
}
