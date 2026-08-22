// StorageClassClient gRPC wrapper.

package client

import (
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// StorageClassClient is the typed wrapper around generated.StorageClassServiceClient.
type StorageClassClient struct {
	client *Client
	logger log.Logger
	svc    generated.StorageClassServiceClient
}

// NewStorageClassClient constructs a StorageClassClient.
func NewStorageClassClient(client *Client) *StorageClassClient {
	return &StorageClassClient{
		client: client,
		logger: client.logger.WithComponent("storageclass-client"),
		svc:    generated.NewStorageClassServiceClient(client.conn),
	}
}

// GetLogger returns the client's logger.
func (c *StorageClassClient) GetLogger() log.Logger { return c.logger }

func (c *StorageClassClient) CreateStorageClass(sc *types.StorageClass) error {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.CreateStorageClass(ctx, &generated.CreateStorageClassRequest{
		StorageClass: StorageClassToProto(sc),
	})
	if err != nil {
		return convertGRPCError("create storage class", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return nil
}

func (c *StorageClassClient) GetStorageClass(name string) (*types.StorageClass, error) {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.GetStorageClass(ctx, &generated.GetStorageClassRequest{Name: name})
	if err != nil {
		st, ok := status.FromError(err)
		if ok && st.Code() == codes.NotFound {
			return nil, fmt.Errorf("storage class not found: %s", name)
		}
		return nil, convertGRPCError("get storage class", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return ProtoToStorageClass(resp.StorageClass), nil
}

func (c *StorageClassClient) UpdateStorageClass(sc *types.StorageClass) error {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.UpdateStorageClass(ctx, &generated.UpdateStorageClassRequest{
		StorageClass: StorageClassToProto(sc),
	})
	if err != nil {
		return convertGRPCError("update storage class", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return nil
}

func (c *StorageClassClient) DeleteStorageClass(name string, cascade bool) error {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.DeleteStorageClass(ctx, &generated.DeleteStorageClassRequest{Name: name, Cascade: cascade})
	if err != nil {
		return convertGRPCError("delete storage class", err)
	}
	if resp.Code != int32(codes.OK) {
		return fmt.Errorf("API error: %s", resp.Message)
	}
	return nil
}

func (c *StorageClassClient) ListStorageClasses(labelSelector map[string]string) ([]*types.StorageClass, error) {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.ListStorageClasses(ctx, &generated.ListStorageClassesRequest{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, convertGRPCError("list storage classes", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	out := make([]*types.StorageClass, 0, len(resp.StorageClasses))
	for _, p := range resp.StorageClasses {
		out = append(out, ProtoToStorageClass(p))
	}
	return out, nil
}

// StorageClassToProto converts a domain StorageClass to its wire form.
func StorageClassToProto(sc *types.StorageClass) *generated.StorageClass {
	if sc == nil {
		return nil
	}
	return &generated.StorageClass{
		Id:                sc.ID,
		Name:              sc.Name,
		Driver:            sc.Driver,
		Parameters:        sc.Parameters,
		ReclaimPolicy:     string(sc.ReclaimPolicy),
		AllowedTopologies: storageClassTopologyToProto(sc.AllowedTopologies),
		Default:           sc.Default,
		Labels:            sc.Labels,
		CreatedAt:         sc.CreatedAt.Format(time.RFC3339),
		UpdatedAt:         sc.UpdatedAt.Format(time.RFC3339),
	}
}

// ProtoToStorageClass converts a wire StorageClass to its domain form.
// CreatedAt / UpdatedAt are parsed when present, ignoring parse errors.
func ProtoToStorageClass(p *generated.StorageClass) *types.StorageClass {
	if p == nil {
		return nil
	}
	sc := &types.StorageClass{
		ID:                p.Id,
		Name:              p.Name,
		Driver:            p.Driver,
		Parameters:        p.Parameters,
		ReclaimPolicy:     types.ReclaimPolicy(p.ReclaimPolicy),
		AllowedTopologies: protoToStorageClassTopology(p.AllowedTopologies),
		Default:           p.Default,
		Labels:            p.Labels,
	}
	if t, err := time.Parse(time.RFC3339, p.CreatedAt); err == nil {
		sc.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, p.UpdatedAt); err == nil {
		sc.UpdatedAt = t
	}
	return sc
}

func storageClassTopologyToProto(in []types.TopologySelector) []*generated.TopologySelector {
	if len(in) == 0 {
		return nil
	}
	out := make([]*generated.TopologySelector, 0, len(in))
	for _, t := range in {
		ts := &generated.TopologySelector{MatchLabels: t.MatchLabels}
		for _, e := range t.MatchExpressions {
			ts.MatchExpressions = append(ts.MatchExpressions, &generated.TopologyMatchExpression{
				Key:      e.Key,
				Operator: string(e.Operator),
				Values:   e.Values,
			})
		}
		out = append(out, ts)
	}
	return out
}

func protoToStorageClassTopology(in []*generated.TopologySelector) []types.TopologySelector {
	if len(in) == 0 {
		return nil
	}
	out := make([]types.TopologySelector, 0, len(in))
	for _, t := range in {
		ts := types.TopologySelector{MatchLabels: t.MatchLabels}
		for _, e := range t.MatchExpressions {
			ts.MatchExpressions = append(ts.MatchExpressions, types.TopologyMatchExpression{
				Key:      e.Key,
				Operator: types.TopologyOperator(e.Operator),
				Values:   e.Values,
			})
		}
		out = append(out, ts)
	}
	return out
}
