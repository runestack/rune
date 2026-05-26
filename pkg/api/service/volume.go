// VolumeService — namespace-scoped gRPC handlers for Volume resources.
package service

import (
	"context"
	"math"
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
	repo          *repos.VolumeRepo
	nsRepo        *repos.NamespaceRepo
	scRepo        *repos.StorageClassRepo
	driverConfigs map[string]map[string]any
	logger        log.Logger
}

// VolumeServiceOption configures optional VolumeService behaviour.
type VolumeServiceOption func(*VolumeService)

// WithDriverConfigs supplies the runefile [storage.drivers] map so the
// service can perform driver-capability lint at the write path (e.g.
// reject Volumes whose AccessMode is not in the driver's
// Capabilities.AccessModes, or whose local-host hostPath is outside the
// runefile allowlist). Optional — when omitted, driver-capability lint
// is skipped and the controller surfaces capability errors at provision
// time instead.
func WithDriverConfigs(cfg map[string]map[string]any) VolumeServiceOption {
	return func(s *VolumeService) { s.driverConfigs = cfg }
}

// NewVolumeService constructs a VolumeService.
func NewVolumeService(st store.Store, logger log.Logger, opts ...VolumeServiceOption) *VolumeService {
	s := &VolumeService{
		repo:   repos.NewVolumeRepo(st),
		nsRepo: repos.NewNamespaceRepo(st),
		scRepo: repos.NewStorageClassRepo(st),
		logger: logger.WithComponent("volume-service"),
	}
	for _, o := range opts {
		o(s)
	}
	return s
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
	if err := s.validateAgainstStorageClass(ctx, v); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
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
	if err := s.validateAgainstStorageClass(ctx, v); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
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
		StatusReason:     v.StatusReason,
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
		StatusReason:     p.StatusReason,
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

// RetryProvisionVolume resets a Failed/Stalled volume back to Pending so
// the controller will attempt provisioning again. Volumes already in any
// other state are rejected with FailedPrecondition.
func (s *VolumeService) RetryProvisionVolume(ctx context.Context, req *generated.RetryProvisionVolumeRequest) (*generated.VolumeResponse, error) {
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
	switch v.Status {
	case types.VolumeStatusFailed, types.VolumeStatusStalled:
		// allowed
	default:
		return nil, status.Errorf(codes.FailedPrecondition,
			"retry-provision: volume %s/%s is in %q (only Failed or Stalled may be retried)",
			v.Namespace, v.Name, v.Status)
	}
	v.Status = types.VolumeStatusPending
	v.StatusReason = ""
	v.Message = ""
	if err := s.repo.Update(ctx, v, store.WithSource(store.EventSourceAPI)); err != nil {
		s.logger.Error("retry-provision update failed", log.Err(err), log.Str("ns", v.Namespace), log.Str("name", v.Name))
		return nil, status.Errorf(codes.Internal, "update: %v", err)
	}
	return &generated.VolumeResponse{
		Volume: volumeToProto(v),
		Status: &generated.Status{Code: int32(codes.OK)},
	}, nil
}

// DetachVolume clears bind state on a volume so a replacement instance can
// attach it. Without force=true the volume must already be Released or
// have no BoundClaim (i.e. it's safe to clear). With force=true the
// caller assumes responsibility for any data-loss risk if the previous
// holder is still alive; bind state is cleared regardless of status and
// the volume is moved to Available.
func (s *VolumeService) DetachVolume(ctx context.Context, req *generated.DetachVolumeRequest) (*generated.VolumeResponse, error) {
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
	if !req.Force {
		// Soft path: refuse to disturb a live binding.
		if v.BoundClaim != "" && v.Status == types.VolumeStatusBound {
			return nil, status.Errorf(codes.FailedPrecondition,
				"detach: volume %s/%s is Bound to %q; pass --force to override (data-loss risk)",
				v.Namespace, v.Name, v.BoundClaim)
		}
	}
	v.BoundClaim = ""
	v.BoundNode = ""
	// Drop OwnerService too so reclaim no longer fires when its parent
	// service is later deleted; an operator who force-detaches has
	// taken explicit ownership.
	v.OwnerService = ""
	if v.Handle != "" {
		v.Status = types.VolumeStatusAvailable
	} else {
		v.Status = types.VolumeStatusPending
	}
	v.StatusReason = ""
	v.Message = ""
	if err := s.repo.Update(ctx, v, store.WithSource(store.EventSourceAPI)); err != nil {
		s.logger.Error("detach update failed", log.Err(err), log.Str("ns", v.Namespace), log.Str("name", v.Name))
		return nil, status.Errorf(codes.Internal, "update: %v", err)
	}
	return &generated.VolumeResponse{
		Volume: volumeToProto(v),
		Status: &generated.Status{Code: int32(codes.OK)},
	}, nil
}
