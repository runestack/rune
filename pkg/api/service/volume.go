// VolumeService — namespace-scoped gRPC handlers for Volume resources.
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

// VolumeService implements generated.VolumeServiceServer.
type VolumeService struct {
	generated.UnimplementedVolumeServiceServer
	repo   *repos.VolumeRepo
	nsRepo *repos.NamespaceRepo
	logger log.Logger
}

// NewVolumeService constructs a VolumeService.
func NewVolumeService(st store.Store, logger log.Logger) *VolumeService {
	return &VolumeService{
		repo:   repos.NewVolumeRepo(st),
		nsRepo: repos.NewNamespaceRepo(st),
		logger: logger.WithComponent("volume-service"),
	}
}

func (s *VolumeService) CreateVolume(ctx context.Context, req *generated.CreateVolumeRequest) (*generated.VolumeResponse, error) {
	if req.Volume == nil {
		return nil, status.Error(codes.InvalidArgument, "volume is required")
	}
	v, err := protoToVolume(req.Volume)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume: %v", err)
	}
	v.Namespace = types.NS(v.Namespace)
	if err := ensureNamespaceExists(ctx, s.nsRepo, v.Namespace, req.EnsureNamespace); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "ensure namespace: %v", err)
	}
	if err := s.repo.Create(ctx, v); err != nil {
		if store.IsAlreadyExistsError(err) {
			return nil, status.Errorf(codes.AlreadyExists, "volume already exists: %s/%s", v.Namespace, v.Name)
		}
		s.logger.Error("create volume failed", log.Err(err), log.Str("ns", v.Namespace), log.Str("name", v.Name))
		return nil, status.Errorf(codes.Internal, "create: %v", err)
	}
	return &generated.VolumeResponse{
		Volume: volumeToProto(v),
		Status: &generated.Status{Code: int32(codes.OK)},
	}, nil
}

func (s *VolumeService) GetVolume(ctx context.Context, req *generated.GetVolumeRequest) (*generated.VolumeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	v, err := s.repo.Get(ctx, types.NS(req.Namespace), req.Name)
	if err != nil {
		if store.IsNotFoundError(err) {
			return nil, status.Errorf(codes.NotFound, "volume not found: %s/%s", req.Namespace, req.Name)
		}
		return nil, status.Errorf(codes.Internal, "get: %v", err)
	}
	return &generated.VolumeResponse{
		Volume: volumeToProto(v),
		Status: &generated.Status{Code: int32(codes.OK)},
	}, nil
}

func (s *VolumeService) UpdateVolume(ctx context.Context, req *generated.UpdateVolumeRequest) (*generated.VolumeResponse, error) {
	if req.Volume == nil {
		return nil, status.Error(codes.InvalidArgument, "volume is required")
	}
	v, err := protoToVolume(req.Volume)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume: %v", err)
	}
	v.Namespace = types.NS(v.Namespace)
	if err := s.repo.Update(ctx, v, store.WithSource(store.EventSourceAPI)); err != nil {
		if store.IsNotFoundError(err) {
			return nil, status.Errorf(codes.NotFound, "volume not found: %s/%s", v.Namespace, v.Name)
		}
		s.logger.Error("update volume failed", log.Err(err), log.Str("ns", v.Namespace), log.Str("name", v.Name))
		return nil, status.Errorf(codes.Internal, "update: %v", err)
	}
	return &generated.VolumeResponse{
		Volume: volumeToProto(v),
		Status: &generated.Status{Code: int32(codes.OK)},
	}, nil
}

func (s *VolumeService) DeleteVolume(ctx context.Context, req *generated.DeleteVolumeRequest) (*generated.Status, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := s.repo.Delete(ctx, types.NS(req.Namespace), req.Name); err != nil {
		if store.IsNotFoundError(err) {
			return nil, status.Errorf(codes.NotFound, "volume not found: %s/%s", req.Namespace, req.Name)
		}
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	return &generated.Status{Code: int32(codes.OK)}, nil
}

func (s *VolumeService) ListVolumes(ctx context.Context, req *generated.ListVolumesRequest) (*generated.ListVolumesResponse, error) {
	vols, err := s.repo.List(ctx, types.NS(req.Namespace))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}
	out := make([]*generated.Volume, 0, len(vols))
	for _, v := range vols {
		if !matchLabels(v.Labels, req.LabelSelector) {
			continue
		}
		if !matchVolumeFieldSelector(v, req.FieldSelector) {
			continue
		}
		out = append(out, volumeToProto(v))
	}
	return &generated.ListVolumesResponse{
		Volumes: out,
		Status:  &generated.Status{Code: int32(codes.OK)},
	}, nil
}

// matchVolumeFieldSelector supports a small set of well-known field paths
// commonly used to filter volume listings.
func matchVolumeFieldSelector(v *types.Volume, sel map[string]string) bool {
	for k, want := range sel {
		var got string
		switch k {
		case "name", "metadata.name":
			got = v.Name
		case "namespace", "metadata.namespace":
			got = v.Namespace
		case "status", "status.phase":
			got = string(v.Status)
		case "storageClassName", "spec.storageClassName":
			got = v.StorageClassName
		case "ownerService":
			got = v.OwnerService
		case "boundNode":
			got = v.BoundNode
		default:
			// Unknown selector keys do not match anything.
			return false
		}
		if got != want {
			return false
		}
	}
	return true
}

func volumeToProto(v *types.Volume) *generated.Volume {
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
		out.SnapshotSchedule = &generated.SnapshotSchedule{
			Cron:      v.SnapshotSchedule.Cron,
			Retention: int32(v.SnapshotSchedule.Retention),
		}
	}
	return out
}

func protoToVolume(p *generated.Volume) (*types.Volume, error) {
	if p == nil {
		return nil, nil
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
	if p.SnapshotSchedule != nil {
		v.SnapshotSchedule = &types.SnapshotSchedule{
			Cron:      p.SnapshotSchedule.Cron,
			Retention: int(p.SnapshotSchedule.Retention),
		}
	}
	return v, nil
}
