package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/store/repos"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ConfigmapService struct {
	generated.UnimplementedConfigmapServiceServer
	repo   *repos.ConfigmapRepo
	nsRepo *repos.NamespaceRepo
	logger log.Logger
	// patchLocks serialises concurrent Patch/Rollback on the same configmap so
	// the read-merge-write stays atomic without touching the store layer.
	patchLocks sync.Map
}

func NewConfigmapService(st store.Store, logger log.Logger) *ConfigmapService {
	return &ConfigmapService{
		repo:   repos.NewConfigRepo(st),
		nsRepo: repos.NewNamespaceRepo(st),
		logger: logger,
	}
}

func (s *ConfigmapService) CreateConfigmap(ctx context.Context, req *generated.CreateConfigmapRequest) (*generated.ConfigmapResponse, error) {
	if req.Configmap == nil {
		return nil, status.Error(codes.InvalidArgument, "config map is required")
	}
	now := time.Now()

	namespace := types.NS(req.Configmap.Namespace)
	err := ensureNamespaceExists(ctx, s.nsRepo, namespace, req.EnsureNamespace)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "failed to ensure namespace exists: %v", err)
	}

	c := &types.Configmap{Name: req.Configmap.Name, Namespace: namespace, Data: req.Configmap.Data, Version: 1, CreatedAt: now, UpdatedAt: now}

	ref := types.FormatRef(types.ResourceTypeConfigmap, c.Namespace, c.Name)
	if err := s.repo.Create(ctx, ref, c); err != nil {
		// If already exists, fall back to update path
		if store.IsAlreadyExistsError(err) {
			s.logger.Info("Config already exists, updating instead",
				log.Str("name", req.Configmap.Name),
				log.Str("namespace", namespace))
			return s.UpdateConfigmap(ctx, &generated.UpdateConfigmapRequest{Configmap: req.Configmap})
		}
		return nil, status.Errorf(codes.Internal, "create: %v", err)
	}
	return &generated.ConfigmapResponse{Configmap: toProtoConfigmap(c), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

func (s *ConfigmapService) GetConfigmap(ctx context.Context, req *generated.GetConfigmapRequest) (*generated.ConfigmapResponse, error) {
	c, err := s.repo.Get(ctx, types.NS(req.Namespace), req.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "get: %v", err)
	}
	return &generated.ConfigmapResponse{Configmap: toProtoConfigmap(c), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

func (s *ConfigmapService) UpdateConfigmap(ctx context.Context, req *generated.UpdateConfigmapRequest) (*generated.ConfigmapResponse, error) {
	if req.Configmap == nil {
		return nil, status.Error(codes.InvalidArgument, "config map is required")
	}
	namespace := types.NS(req.Configmap.Namespace)

	// Fetch current; if missing, return NotFound
	current, err := s.repo.Get(ctx, namespace, req.Configmap.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "config not found: %s/%s", namespace, req.Configmap.Name)
	}

	desired := &types.Configmap{Name: req.Configmap.Name, Namespace: namespace, Data: req.Configmap.Data}

	// If unchanged, no-op
	if reflect.DeepEqual(current.Data, desired.Data) {
		s.logger.Info("Config unchanged; no update", log.Str("name", desired.Name), log.Str("namespace", desired.Namespace))
		return &generated.ConfigmapResponse{Configmap: toProtoConfigmap(current), Status: &generated.Status{Code: int32(codes.OK)}}, nil
	}

	// For observability, compute hashes
	oldHash := hashConfig(current)
	newHash := hashConfig(desired)
	s.logger.Info("Updating config",
		log.Str("name", desired.Name), log.Str("namespace", desired.Namespace),
		log.Str("old_hash", oldHash[:8]), log.Str("new_hash", newHash[:8]))

	if err := s.repo.Update(ctx, namespace, req.Configmap.Name, desired, store.WithSource(store.EventSourceAPI)); err != nil {
		return nil, status.Errorf(codes.Internal, "update: %v", err)
	}
	got, _ := s.repo.Get(ctx, namespace, req.Configmap.Name)
	return &generated.ConfigmapResponse{Configmap: toProtoConfigmap(got), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// PatchConfigmap applies a key-scoped merge (set/unset) to a configmap's data
// under a per-configmap lock, producing a new version. Mirrors PatchSecret but
// without encryption or audit (configmaps are plaintext).
func (s *ConfigmapService) PatchConfigmap(ctx context.Context, req *generated.PatchConfigmapRequest) (*generated.ConfigmapResponse, error) {
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if len(req.Set) == 0 && len(req.Unset) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one of set or unset must be non-empty")
	}
	namespace := types.NS(req.Namespace)

	mu := s.acquirePatchLock(namespace, req.Name)
	mu.Lock()
	defer mu.Unlock()

	current, err := s.repo.Get(ctx, namespace, req.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "configmap not found: %s/%s", namespace, req.Name)
	}

	merged := make(map[string]string, len(current.Data)+len(req.Set))
	for k, v := range current.Data {
		merged[k] = v
	}
	for k, v := range req.Set {
		merged[k] = v
	}
	for _, k := range req.Unset {
		delete(merged, k)
	}
	desired := &types.Configmap{Name: req.Name, Namespace: namespace, Data: merged}

	if reflect.DeepEqual(current.Data, desired.Data) {
		s.logger.Info("Configmap patch is a no-op; no update", log.Str("name", desired.Name), log.Str("namespace", desired.Namespace))
		return &generated.ConfigmapResponse{Configmap: toProtoConfigmap(current), Status: &generated.Status{Code: int32(codes.OK)}}, nil
	}

	oldHash := hashConfig(current)
	newHash := hashConfig(desired)
	s.logger.Info("Patching configmap",
		log.Str("name", desired.Name), log.Str("namespace", desired.Namespace),
		log.Str("old_hash", oldHash[:8]), log.Str("new_hash", newHash[:8]),
		log.Int("set", len(req.Set)), log.Int("unset", len(req.Unset)))

	if err := s.repo.Update(ctx, namespace, req.Name, desired, store.WithSource(store.EventSourceAPI)); err != nil {
		return nil, status.Errorf(codes.Internal, "patch: %v", err)
	}
	got, _ := s.repo.Get(ctx, namespace, req.Name)
	return &generated.ConfigmapResponse{Configmap: toProtoConfigmap(got), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// ListConfigmapVersions returns the configmap's version history, newest first.
func (s *ConfigmapService) ListConfigmapVersions(ctx context.Context, req *generated.ListConfigmapVersionsRequest) (*generated.ListConfigmapVersionsResponse, error) {
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	namespace := types.NS(req.Namespace)
	versions, err := s.repo.ListVersions(ctx, namespace, req.Name)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list versions: %v", err)
	}
	out := make([]*generated.Configmap, 0, len(versions))
	for _, c := range versions {
		out = append(out, toProtoConfigmap(c))
	}
	return &generated.ListConfigmapVersionsResponse{Versions: out, Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// RollbackConfigmap rewrites HEAD to the data of a prior version (head+1).
func (s *ConfigmapService) RollbackConfigmap(ctx context.Context, req *generated.RollbackConfigmapRequest) (*generated.ConfigmapResponse, error) {
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	toVersion := int(req.ToVersion)
	if toVersion <= 0 {
		return nil, status.Error(codes.InvalidArgument, "to_version must be > 0")
	}
	namespace := types.NS(req.Namespace)

	mu := s.acquirePatchLock(namespace, req.Name)
	mu.Lock()
	defer mu.Unlock()

	current, err := s.repo.Get(ctx, namespace, req.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "configmap not found: %s/%s", namespace, req.Name)
	}
	if toVersion == current.Version {
		return nil, status.Errorf(codes.InvalidArgument, "configmap %s/%s is already at version %d", namespace, req.Name, toVersion)
	}
	rolled, err := s.repo.Rollback(ctx, namespace, req.Name, toVersion, store.WithSource(store.EventSourceAPI))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "rollback: %v", err)
	}
	return &generated.ConfigmapResponse{Configmap: toProtoConfigmap(rolled), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// acquirePatchLock returns the per-configmap mutex, lazily creating it.
func (s *ConfigmapService) acquirePatchLock(namespace, name string) *sync.Mutex {
	key := namespace + "/" + name
	mu, _ := s.patchLocks.LoadOrStore(key, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func (s *ConfigmapService) DeleteConfigmap(ctx context.Context, req *generated.DeleteConfigmapRequest) (*generated.Status, error) {
	if err := s.repo.Delete(ctx, req.Namespace, req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	return &generated.Status{Code: int32(codes.OK)}, nil
}

func (s *ConfigmapService) ListConfigmaps(ctx context.Context, req *generated.ListConfigmapsRequest) (*generated.ListConfigmapsResponse, error) {
	// BaseRepo exposes List; ConfigmapRepo does not add a wrapper, so call through base
	configs, err := s.repo.List(ctx, types.NS(req.Namespace))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}
	out := make([]*generated.Configmap, 0, len(configs))
	for _, c := range configs {
		out = append(out, toProtoConfigmap(c))
	}
	return &generated.ListConfigmapsResponse{Configmaps: out, Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

func toProtoConfigmap(c *types.Configmap) *generated.Configmap {
	return &generated.Configmap{
		Name:      c.Name,
		Namespace: c.Namespace,
		Data:      c.Data,
		Version:   utils.ToInt32NonNegative(c.Version),
		CreatedAt: c.CreatedAt.Format(time.RFC3339),
		UpdatedAt: c.UpdatedAt.Format(time.RFC3339),
	}
}

// hashConfig returns a deterministic hash for comparing config content
func hashConfig(c *types.Configmap) string {
	payload := struct {
		Data map[string]string `json:"data"`
	}{Data: c.Data}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}
