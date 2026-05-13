// SnapshotService — namespace-scoped gRPC handlers for Snapshot resources.
package service

import (
	"context"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/storage/driver"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RestoreFromSnapshotParam is re-exported for compatibility; the
// canonical definition lives in pkg/types/snapshot.go.
const RestoreFromSnapshotParam = types.RestoreFromSnapshotParam

// SnapshotService implements generated.SnapshotServiceServer.
type SnapshotService struct {
	generated.UnimplementedSnapshotServiceServer
	repo          *repos.SnapshotRepo
	volRepo       *repos.VolumeRepo
	nsRepo        *repos.NamespaceRepo
	scRepo        *repos.StorageClassRepo
	driverConfigs map[string]map[string]any
	logger        log.Logger
}

// SnapshotServiceOption configures optional SnapshotService behaviour.
type SnapshotServiceOption func(*SnapshotService)

// WithSnapshotDriverConfigs supplies the runefile [storage.drivers] map so
// the service can perform driver-capability lint at the write path
// (reject CreateSnapshot when the source volume's driver does not
// declare Capabilities.Snapshots).
func WithSnapshotDriverConfigs(cfg map[string]map[string]any) SnapshotServiceOption {
	return func(s *SnapshotService) { s.driverConfigs = cfg }
}

// NewSnapshotService constructs a SnapshotService.
func NewSnapshotService(st store.Store, logger log.Logger, opts ...SnapshotServiceOption) *SnapshotService {
	s := &SnapshotService{
		repo:    repos.NewSnapshotRepo(st),
		volRepo: repos.NewVolumeRepo(st),
		nsRepo:  repos.NewNamespaceRepo(st),
		scRepo:  repos.NewStorageClassRepo(st),
		logger:  logger.WithComponent("snapshot-service"),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *SnapshotService) CreateSnapshot(ctx context.Context, req *generated.CreateSnapshotRequest) (*generated.SnapshotResponse, error) {
	if req.Snapshot == nil {
		return nil, status.Error(codes.InvalidArgument, "snapshot is required")
	}
	snap := protoToSnapshot(req.Snapshot)
	snap.Namespace = types.NS(snap.Namespace)
	if err := ensureNamespaceExists(ctx, s.nsRepo, snap.Namespace, req.EnsureNamespace); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "ensure namespace: %v", err)
	}
	if snap.SourceVolume == "" {
		return nil, status.Error(codes.InvalidArgument, "sourceVolume is required")
	}
	// Verify the source Volume exists; capture its driver onto the
	// Snapshot so the controller doesn't have to re-resolve at delete time.
	src, err := s.volRepo.Get(ctx, snap.Namespace, snap.SourceVolume)
	if err != nil {
		if store.IsNotFoundError(err) {
			return nil, status.Errorf(codes.FailedPrecondition,
				"source volume %s/%s not found", snap.Namespace, snap.SourceVolume)
		}
		return nil, status.Errorf(codes.Internal, "lookup source volume: %v", err)
	}
	// Driver-capability lint: reject if the source volume's class
	// declares Capabilities.Snapshots:false. Fail-OPEN matches the
	// volume_lint policy: missing class / unregistered driver / factory
	// error → defer to the controller.
	if snap.Driver == "" {
		if sc, err := s.scRepo.Get(ctx, src.StorageClassName); err == nil {
			snap.Driver = sc.Driver
			if caps, ok := s.lookupSnapshotCaps(sc.Driver); ok && !caps.Snapshots {
				return nil, status.Errorf(codes.InvalidArgument,
					"snapshot: driver %q does not support snapshots", sc.Driver)
			}
		}
	}
	if err := s.repo.Create(ctx, snap); err != nil {
		if store.IsAlreadyExistsError(err) {
			return nil, status.Errorf(codes.AlreadyExists, "snapshot already exists: %s/%s", snap.Namespace, snap.Name)
		}
		s.logger.Error("create snapshot failed", log.Err(err), log.Str("ns", snap.Namespace), log.Str("name", snap.Name))
		return nil, status.Errorf(codes.Internal, "create: %v", err)
	}
	return &generated.SnapshotResponse{
		Snapshot: snapshotToProto(snap),
		Status:   &generated.Status{Code: int32(codes.OK)},
	}, nil
}

func (s *SnapshotService) GetSnapshot(ctx context.Context, req *generated.GetSnapshotRequest) (*generated.SnapshotResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	snap, err := s.repo.Get(ctx, types.NS(req.Namespace), req.Name)
	if err != nil {
		if store.IsNotFoundError(err) {
			return nil, status.Errorf(codes.NotFound, "snapshot not found: %s/%s", req.Namespace, req.Name)
		}
		return nil, status.Errorf(codes.Internal, "get: %v", err)
	}
	return &generated.SnapshotResponse{
		Snapshot: snapshotToProto(snap),
		Status:   &generated.Status{Code: int32(codes.OK)},
	}, nil
}

func (s *SnapshotService) DeleteSnapshot(ctx context.Context, req *generated.DeleteSnapshotRequest) (*generated.Status, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	ns := types.NS(req.Namespace)
	// Two-phase delete: flip to Deleting (so the controller drives
	// driver.DeleteSnapshot), and then the controller removes the row
	// once the driver call succeeds. Falls through to a hard delete if
	// the snapshot is already in a non-driver-owned state (Pending or
	// Failed with no Handle).
	snap, err := s.repo.Get(ctx, ns, req.Name)
	if err != nil {
		if store.IsNotFoundError(err) {
			return nil, status.Errorf(codes.NotFound, "snapshot not found: %s/%s", req.Namespace, req.Name)
		}
		return nil, status.Errorf(codes.Internal, "get: %v", err)
	}
	if snap.Handle == "" || snap.Phase == types.SnapshotPhasePending || snap.Phase == types.SnapshotPhaseFailed {
		// Nothing for the driver to delete. Drop the row immediately.
		if err := s.repo.Delete(ctx, ns, req.Name); err != nil {
			return nil, status.Errorf(codes.Internal, "delete: %v", err)
		}
		return &generated.Status{Code: int32(codes.OK)}, nil
	}
	snap.Phase = types.SnapshotPhaseDeleting
	snap.Reason = ""
	snap.Message = ""
	if err := s.repo.Update(ctx, snap, store.WithSource(store.EventSourceAPI)); err != nil {
		return nil, status.Errorf(codes.Internal, "update: %v", err)
	}
	return &generated.Status{Code: int32(codes.OK)}, nil
}

func (s *SnapshotService) ListSnapshots(ctx context.Context, req *generated.ListSnapshotsRequest) (*generated.ListSnapshotsResponse, error) {
	ns := types.NS(req.Namespace)
	var snaps []*types.Snapshot
	var err error
	if ns == "*" {
		snaps, err = s.repo.ListAll(ctx)
	} else {
		snaps, err = s.repo.List(ctx, ns)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}
	out := make([]*generated.Snapshot, 0, len(snaps))
	for _, sn := range snaps {
		if !matchLabels(sn.Labels, req.LabelSelector) {
			continue
		}
		out = append(out, snapshotToProto(sn))
	}
	return &generated.ListSnapshotsResponse{
		Snapshots: out,
		Status:    &generated.Status{Code: int32(codes.OK)},
	}, nil
}

// RestoreVolume provisions a new Volume from this Snapshot. The new
// Volume starts in Pending with a Parameters["rune.io/restoreFromSnapshot"]
// stamp; the VolumeController routes that to Driver.RestoreFromSnapshot
// instead of Driver.Provision on first reconcile.
func (s *SnapshotService) RestoreVolume(ctx context.Context, req *generated.RestoreVolumeRequest) (*generated.VolumeResponse, error) {
	if req.SnapshotName == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot_name is required")
	}
	if req.TargetVolumeName == "" {
		return nil, status.Error(codes.InvalidArgument, "target_volume_name is required")
	}
	snapNS := types.NS(req.SnapshotNamespace)
	snap, err := s.repo.Get(ctx, snapNS, req.SnapshotName)
	if err != nil {
		if store.IsNotFoundError(err) {
			return nil, status.Errorf(codes.NotFound, "snapshot not found: %s/%s", snapNS, req.SnapshotName)
		}
		return nil, status.Errorf(codes.Internal, "get snapshot: %v", err)
	}
	if snap.Phase != types.SnapshotPhaseReady {
		return nil, status.Errorf(codes.FailedPrecondition,
			"snapshot %s/%s is in phase %q (must be Ready to restore)", snap.Namespace, snap.Name, snap.Phase)
	}
	src, err := s.volRepo.Get(ctx, snapNS, snap.SourceVolume)
	if err != nil && !store.IsNotFoundError(err) {
		return nil, status.Errorf(codes.Internal, "get source volume: %v", err)
	}
	scName := req.StorageClassName
	if scName == "" && src != nil {
		scName = src.StorageClassName
	}
	targetNS := req.TargetNamespace
	if targetNS == "" {
		targetNS = snapNS
	}
	params := map[string]string{RestoreFromSnapshotParam: snap.Namespace + "/" + snap.Name}
	if src != nil {
		for k, v := range src.Parameters {
			if _, exists := params[k]; !exists {
				params[k] = v
			}
		}
	}
	target := &types.Volume{
		Name:             req.TargetVolumeName,
		Namespace:        targetNS,
		Labels:           req.Labels,
		StorageClassName: scName,
		Parameters:       params,
		Status:           types.VolumeStatusPending,
	}
	if src != nil {
		if target.Size == "" {
			target.Size = src.Size
		}
		if target.AccessMode == "" {
			target.AccessMode = src.AccessMode
		}
		if target.ReclaimPolicy == "" {
			target.ReclaimPolicy = src.ReclaimPolicy
		}
	}
	if err := ensureNamespaceExists(ctx, s.nsRepo, targetNS, false); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "ensure target namespace: %v", err)
	}
	if err := s.volRepo.Create(ctx, target); err != nil {
		if store.IsAlreadyExistsError(err) {
			return nil, status.Errorf(codes.AlreadyExists, "target volume already exists: %s/%s", target.Namespace, target.Name)
		}
		return nil, status.Errorf(codes.Internal, "create target volume: %v", err)
	}
	return &generated.VolumeResponse{
		Volume: volumeToProto(target),
		Status: &generated.Status{Code: int32(codes.OK)},
	}, nil
}

// lookupSnapshotCaps mirrors VolumeService.lookupCapabilities but is
// duplicated locally so the snapshot path doesn't require a refactor of
// the volume_lint helpers. Returns (caps, true) when the driver could be
// instantiated; (zero, false) on any failure (fail-OPEN).
func (s *SnapshotService) lookupSnapshotCaps(driverName string) (driver.Capabilities, bool) {
	cfg := s.driverConfigs[driverName]
	d, err := driver.New(driverName, cfg)
	if err != nil {
		return driver.Capabilities{}, false
	}
	return d.Capabilities(), true
}

func snapshotToProto(s *types.Snapshot) *generated.Snapshot {
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

func protoToSnapshot(p *generated.Snapshot) *types.Snapshot {
	if p == nil {
		return nil
	}
	return &types.Snapshot{
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
}
