// VolumeClient — typed wrapper around generated.VolumeServiceClient. RUNE-072.
package client

import (
	"fmt"
	"math"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// VolumeClient is the typed wrapper around generated.VolumeServiceClient.
type VolumeClient struct {
	client *Client
	logger log.Logger
	svc    generated.VolumeServiceClient
}

// NewVolumeClient constructs a VolumeClient.
func NewVolumeClient(c *Client) *VolumeClient {
	return &VolumeClient{
		client: c,
		logger: c.logger.WithComponent("volume-client"),
		svc:    generated.NewVolumeServiceClient(c.conn),
	}
}

// GetLogger returns the client's logger.
func (c *VolumeClient) GetLogger() log.Logger { return c.logger }

func (c *VolumeClient) CreateVolume(v *types.Volume, ensureNamespace bool) error {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.CreateVolume(ctx, &generated.CreateVolumeRequest{
		Volume:          VolumeToProto(v),
		EnsureNamespace: ensureNamespace,
	})
	if err != nil {
		return convertGRPCError("create volume", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return nil
}

func (c *VolumeClient) GetVolume(namespace, name string) (*types.Volume, error) {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.GetVolume(ctx, &generated.GetVolumeRequest{Name: name, Namespace: namespace})
	if err != nil {
		st, ok := status.FromError(err)
		if ok && st.Code() == codes.NotFound {
			return nil, fmt.Errorf("volume not found: %s/%s", namespace, name)
		}
		return nil, convertGRPCError("get volume", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return ProtoToVolume(resp.Volume), nil
}

func (c *VolumeClient) UpdateVolume(v *types.Volume) error {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.UpdateVolume(ctx, &generated.UpdateVolumeRequest{Volume: VolumeToProto(v)})
	if err != nil {
		return convertGRPCError("update volume", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return nil
}

func (c *VolumeClient) DeleteVolume(namespace, name string) error {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.DeleteVolume(ctx, &generated.DeleteVolumeRequest{Name: name, Namespace: namespace})
	if err != nil {
		return convertGRPCError("delete volume", err)
	}
	if resp.Code != int32(codes.OK) {
		return fmt.Errorf("API error: %s", resp.Message)
	}
	return nil
}

func (c *VolumeClient) ListVolumes(namespace string, labelSelector, fieldSelector map[string]string) ([]*types.Volume, error) {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.ListVolumes(ctx, &generated.ListVolumesRequest{
		Namespace:     namespace,
		LabelSelector: labelSelector,
		FieldSelector: fieldSelector,
	})
	if err != nil {
		return nil, convertGRPCError("list volumes", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	out := make([]*types.Volume, 0, len(resp.Volumes))
	for _, p := range resp.Volumes {
		out = append(out, ProtoToVolume(p))
	}
	return out, nil
}

// RetryProvisionVolume re-arms a Failed/Stalled volume by transitioning
// it back to Pending. Returns the updated Volume.
func (c *VolumeClient) RetryProvisionVolume(namespace, name string) (*types.Volume, error) {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.RetryProvisionVolume(ctx, &generated.RetryProvisionVolumeRequest{Name: name, Namespace: namespace})
	if err != nil {
		return nil, convertGRPCError("retry-provision volume", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return ProtoToVolume(resp.Volume), nil
}

// DetachVolume clears bind state on a volume so a replacement instance
// can attach it. With force=true the call is allowed regardless of
// current status.
func (c *VolumeClient) DetachVolume(namespace, name string, force bool) (*types.Volume, error) {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.DetachVolume(ctx, &generated.DetachVolumeRequest{Name: name, Namespace: namespace, Force: force})
	if err != nil {
		return nil, convertGRPCError("detach volume", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return ProtoToVolume(resp.Volume), nil
}

// VolumeToProto converts a domain Volume to its wire form.
func VolumeToProto(v *types.Volume) *generated.Volume {
	if v == nil {
		return nil
	}
	out := &generated.Volume{
		Id:               v.ID,
		Name:             v.Name,
		Namespace:        v.Namespace,
		Labels:           v.Labels,
		StorageClassName: v.StorageClassName,
		Size:             v.Size,
		AccessMode:       string(v.AccessMode),
		ReclaimPolicy:    string(v.ReclaimPolicy),
		Parameters:       v.Parameters,
		Handle:           v.Handle,
		OwnerService:     v.OwnerService,
		BoundNode:        v.BoundNode,
		BoundClaim:       v.BoundClaim,
		Status:           string(v.Status),
		Reason:           v.Reason,
		Message:          v.Message,
		CreatedAt:        v.CreatedAt.Format(time.RFC3339),
		UpdatedAt:        v.UpdatedAt.Format(time.RFC3339),
	}
	if v.SnapshotSchedule != nil {
		ret := v.SnapshotSchedule.Retention
		if ret < 0 {
			ret = 0
		} else if ret > math.MaxInt32 {
			ret = math.MaxInt32
		}
		out.SnapshotSchedule = &generated.SnapshotSchedule{
			Cron:      v.SnapshotSchedule.Cron,
			Retention: int32(ret), //nolint:gosec // bounds-checked above
		}
	}
	return out
}

// ProtoToVolume converts a wire Volume back to its domain form.
func ProtoToVolume(p *generated.Volume) *types.Volume {
	if p == nil {
		return nil
	}
	v := &types.Volume{
		ID:               p.Id,
		Name:             p.Name,
		Namespace:        p.Namespace,
		Labels:           p.Labels,
		StorageClassName: p.StorageClassName,
		Size:             p.Size,
		AccessMode:       types.AccessMode(p.AccessMode),
		ReclaimPolicy:    types.ReclaimPolicy(p.ReclaimPolicy),
		Parameters:       p.Parameters,
		Handle:           p.Handle,
		OwnerService:     p.OwnerService,
		BoundNode:        p.BoundNode,
		BoundClaim:       p.BoundClaim,
		Status:           types.VolumeStatus(p.Status),
		Reason:           p.Reason,
		Message:          p.Message,
	}
	if t, err := time.Parse(time.RFC3339, p.CreatedAt); err == nil {
		v.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, p.UpdatedAt); err == nil {
		v.UpdatedAt = t
	}
	if p.SnapshotSchedule != nil {
		v.SnapshotSchedule = &types.SnapshotSchedule{
			Cron:      p.SnapshotSchedule.Cron,
			Retention: int(p.SnapshotSchedule.Retention),
		}
	}
	return v
}
