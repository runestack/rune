// Package service — StorageClassService gRPC handlers. RUNE-072.
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

// StorageClassService implements generated.StorageClassServiceServer.
// StorageClass is cluster-scoped (no namespace).
type StorageClassService struct {
	generated.UnimplementedStorageClassServiceServer
	repo   *repos.StorageClassRepo
	logger log.Logger
}

// NewStorageClassService constructs a StorageClassService.
func NewStorageClassService(st store.Store, logger log.Logger) *StorageClassService {
	return &StorageClassService{
		repo:   repos.NewStorageClassRepo(st),
		logger: logger.WithComponent("storageclass-service"),
	}
}

func (s *StorageClassService) CreateStorageClass(ctx context.Context, req *generated.CreateStorageClassRequest) (*generated.StorageClassResponse, error) {
	if req.StorageClass == nil {
		return nil, status.Error(codes.InvalidArgument, "storage_class is required")
	}
	sc, err := protoToStorageClass(req.StorageClass)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid storage_class: %v", err)
	}
	if err := s.repo.Create(ctx, sc); err != nil {
		if store.IsAlreadyExistsError(err) {
			return nil, status.Errorf(codes.AlreadyExists, "storage class already exists: %s", sc.Name)
		}
		s.logger.Error("Failed to create storage class", log.Err(err), log.Str("name", sc.Name))
		return nil, status.Errorf(codes.Internal, "create: %v", err)
	}
	return &generated.StorageClassResponse{
		StorageClass: storageClassToProto(sc),
		Status:       &generated.Status{Code: int32(codes.OK)},
	}, nil
}

func (s *StorageClassService) GetStorageClass(ctx context.Context, req *generated.GetStorageClassRequest) (*generated.StorageClassResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	sc, err := s.repo.Get(ctx, req.Name)
	if err != nil {
		if store.IsNotFoundError(err) {
			return nil, status.Errorf(codes.NotFound, "storage class not found: %s", req.Name)
		}
		return nil, status.Errorf(codes.Internal, "get: %v", err)
	}
	return &generated.StorageClassResponse{
		StorageClass: storageClassToProto(sc),
		Status:       &generated.Status{Code: int32(codes.OK)},
	}, nil
}

func (s *StorageClassService) UpdateStorageClass(ctx context.Context, req *generated.UpdateStorageClassRequest) (*generated.StorageClassResponse, error) {
	if req.StorageClass == nil {
		return nil, status.Error(codes.InvalidArgument, "storage_class is required")
	}
	sc, err := protoToStorageClass(req.StorageClass)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid storage_class: %v", err)
	}
	if err := s.repo.Update(ctx, sc, store.WithSource(store.EventSourceAPI)); err != nil {
		if store.IsNotFoundError(err) {
			return nil, status.Errorf(codes.NotFound, "storage class not found: %s", sc.Name)
		}
		s.logger.Error("Failed to update storage class", log.Err(err), log.Str("name", sc.Name))
		return nil, status.Errorf(codes.Internal, "update: %v", err)
	}
	return &generated.StorageClassResponse{
		StorageClass: storageClassToProto(sc),
		Status:       &generated.Status{Code: int32(codes.OK)},
	}, nil
}

func (s *StorageClassService) DeleteStorageClass(ctx context.Context, req *generated.DeleteStorageClassRequest) (*generated.Status, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if err := s.repo.Delete(ctx, req.Name); err != nil {
		if store.IsNotFoundError(err) {
			return nil, status.Errorf(codes.NotFound, "storage class not found: %s", req.Name)
		}
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	return &generated.Status{Code: int32(codes.OK)}, nil
}

func (s *StorageClassService) ListStorageClasses(ctx context.Context, req *generated.ListStorageClassesRequest) (*generated.ListStorageClassesResponse, error) {
	classes, err := s.repo.List(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}
	out := make([]*generated.StorageClass, 0, len(classes))
	for _, sc := range classes {
		if !matchLabels(sc.Labels, req.LabelSelector) {
			continue
		}
		out = append(out, storageClassToProto(sc))
	}
	return &generated.ListStorageClassesResponse{
		StorageClasses: out,
		Status:         &generated.Status{Code: int32(codes.OK)},
	}, nil
}

// matchLabels returns true if every key/value in selector is present in
// labels. An empty selector matches everything.
func matchLabels(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// storageClassToProto converts the domain type to the wire form.
func storageClassToProto(sc *types.StorageClass) *generated.StorageClass {
	if sc == nil {
		return nil
	}
	return &generated.StorageClass{
		Id:                sc.ID,
		Name:              sc.Name,
		Driver:            sc.Driver,
		Parameters:        sc.Parameters,
		ReclaimPolicy:     string(sc.ReclaimPolicy),
		AllowedTopologies: topologySelectorsToProto(sc.AllowedTopologies),
		Default:           sc.Default,
		Labels:            sc.Labels,
		CreatedAt:         sc.CreatedAt.Format(time.RFC3339),
		UpdatedAt:         sc.UpdatedAt.Format(time.RFC3339),
	}
}

// protoToStorageClass converts the wire form back to the domain type.
// CreatedAt / UpdatedAt are not parsed back: the repo manages them.
func protoToStorageClass(p *generated.StorageClass) (*types.StorageClass, error) {
	if p == nil {
		return nil, nil
	}
	sc := &types.StorageClass{
		ID:                p.Id,
		Name:              p.Name,
		Driver:            p.Driver,
		Parameters:        p.Parameters,
		ReclaimPolicy:     types.ReclaimPolicy(p.ReclaimPolicy),
		AllowedTopologies: protoToTopologySelectors(p.AllowedTopologies),
		Default:           p.Default,
		Labels:            p.Labels,
	}
	return sc, nil
}

func topologySelectorsToProto(in []types.TopologySelector) []*generated.TopologySelector {
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

func protoToTopologySelectors(in []*generated.TopologySelector) []types.TopologySelector {
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
