// SnapshotClient — typed wrapper around generated.SnapshotServiceClient. RUNE-071.
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

// SnapshotClient is the typed wrapper around generated.SnapshotServiceClient.
type SnapshotClient struct {
	client *Client
	logger log.Logger
	svc    generated.SnapshotServiceClient
}

// NewSnapshotClient constructs a SnapshotClient.
func NewSnapshotClient(c *Client) *SnapshotClient {
	return &SnapshotClient{
		client: c,
		logger: c.logger.WithComponent("snapshot-client"),
		svc:    generated.NewSnapshotServiceClient(c.conn),
	}
}

// GetLogger returns the client's logger.
func (c *SnapshotClient) GetLogger() log.Logger { return c.logger }

func (c *SnapshotClient) CreateSnapshot(s *types.Snapshot, ensureNamespace bool) (*types.Snapshot, error) {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.CreateSnapshot(ctx, &generated.CreateSnapshotRequest{
		Snapshot:        SnapshotToProto(s),
		EnsureNamespace: ensureNamespace,
	})
	if err != nil {
		return nil, convertGRPCError("create snapshot", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return ProtoToSnapshot(resp.Snapshot), nil
}

func (c *SnapshotClient) GetSnapshot(namespace, name string) (*types.Snapshot, error) {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.GetSnapshot(ctx, &generated.GetSnapshotRequest{Name: name, Namespace: namespace})
	if err != nil {
		st, ok := status.FromError(err)
		if ok && st.Code() == codes.NotFound {
			return nil, fmt.Errorf("snapshot not found: %s/%s", namespace, name)
		}
		return nil, convertGRPCError("get snapshot", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return ProtoToSnapshot(resp.Snapshot), nil
}

func (c *SnapshotClient) DeleteSnapshot(namespace, name string) error {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.DeleteSnapshot(ctx, &generated.DeleteSnapshotRequest{Name: name, Namespace: namespace})
	if err != nil {
		return convertGRPCError("delete snapshot", err)
	}
	if resp.Code != int32(codes.OK) {
		return fmt.Errorf("API error: %s", resp.Message)
	}
	return nil
}

func (c *SnapshotClient) ListSnapshots(namespace string, labelSelector map[string]string) ([]*types.Snapshot, error) {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.ListSnapshots(ctx, &generated.ListSnapshotsRequest{
		Namespace:     namespace,
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, convertGRPCError("list snapshots", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	out := make([]*types.Snapshot, 0, len(resp.Snapshots))
	for _, p := range resp.Snapshots {
		out = append(out, ProtoToSnapshot(p))
	}
	return out, nil
}

// RestoreVolume creates a new Volume from this Snapshot. Returns the new
// Volume row in Pending; the VolumeController drives it through
// driver.RestoreFromSnapshot to Available.
func (c *SnapshotClient) RestoreVolume(snapshotNamespace, snapshotName, targetVolumeName, targetNamespace, storageClassName string, labels map[string]string) (*types.Volume, error) {
	ctx, cancel := c.client.Context()
	defer cancel()
	resp, err := c.svc.RestoreVolume(ctx, &generated.RestoreVolumeRequest{
		SnapshotName:      snapshotName,
		SnapshotNamespace: snapshotNamespace,
		TargetVolumeName:  targetVolumeName,
		TargetNamespace:   targetNamespace,
		Labels:            labels,
		StorageClassName:  storageClassName,
	})
	if err != nil {
		return nil, convertGRPCError("restore volume", err)
	}
	if resp.Status != nil && resp.Status.Code != int32(codes.OK) {
		return nil, fmt.Errorf("API error: %s", resp.Status.Message)
	}
	return ProtoToVolume(resp.Volume), nil
}

// SnapshotToProto converts a types.Snapshot to its proto wire form.
func SnapshotToProto(s *types.Snapshot) *generated.Snapshot {
	if s == nil {
		return nil
	}
	return &generated.Snapshot{
		Id:           s.ID,
		Name:         s.Name,
		Namespace:    s.Namespace,
		Labels:       s.Labels,
		SourceVolume: s.SourceVolume,
		Driver:       s.Driver,
		Handle:       s.Handle,
		SizeBytes:    s.SizeBytes,
		Scheduled:    s.Scheduled,
		Phase:        string(s.Phase),
		Reason:       s.Reason,
		Message:      s.Message,
		CreatedAt:    s.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    s.UpdatedAt.Format(time.RFC3339),
	}
}

// ProtoToSnapshot converts the proto wire form back into a types.Snapshot.
// Best-effort timestamp parsing — falls back to zero values if the proto
// strings aren't RFC3339.
func ProtoToSnapshot(p *generated.Snapshot) *types.Snapshot {
	if p == nil {
		return nil
	}
	out := &types.Snapshot{
		ID:           p.Id,
		Name:         p.Name,
		Namespace:    p.Namespace,
		Labels:       p.Labels,
		SourceVolume: p.SourceVolume,
		Driver:       p.Driver,
		Handle:       p.Handle,
		SizeBytes:    p.SizeBytes,
		Scheduled:    p.Scheduled,
		Phase:        types.SnapshotPhase(p.Phase),
		Reason:       p.Reason,
		Message:      p.Message,
	}
	if p.CreatedAt != "" {
		if t, err := time.Parse(time.RFC3339, p.CreatedAt); err == nil {
			out.CreatedAt = t
		}
	}
	if p.UpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, p.UpdatedAt); err == nil {
			out.UpdatedAt = t
		}
	}
	return out
}
