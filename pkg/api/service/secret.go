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

type SecretService struct {
	generated.UnimplementedSecretServiceServer
	repo   *repos.SecretRepo
	nsRepo *repos.NamespaceRepo
	logger log.Logger

	limiter *tokenBucket
}

func NewSecretService(coreStore store.Store, logger log.Logger) *SecretService {
	return &SecretService{
		repo:    repos.NewSecretRepo(coreStore),
		nsRepo:  repos.NewNamespaceRepo(coreStore),
		logger:  logger,
		limiter: newTokenBucket(20, 20), // 20 requests per second burst 20
	}
}

func (s *SecretService) CreateSecret(ctx context.Context, req *generated.CreateSecretRequest) (*generated.SecretResponse, error) {
	s.logger.Info("Creating secret", log.Str("name", req.Secret.Name), log.Str("namespace", req.Secret.Namespace))
	if req.Secret == nil {
		return nil, status.Error(codes.InvalidArgument, "secret is required")
	}
	now := time.Now()
	sec := &types.Secret{
		Name:      req.Secret.Name,
		Namespace: types.NS(req.Secret.Namespace),
		Type:      req.Secret.Type,
		Data:      req.Secret.Data,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}

	err := ensureNamespaceExists(ctx, s.nsRepo, sec.Namespace, req.EnsureNamespace)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "failed to ensure namespace exists: %v", err)
	}
	if err := s.repo.Create(ctx, sec); err != nil {
		// If already exists, fall back to update path
		if store.IsAlreadyExistsError(err) {
			s.logger.Info("Secret already exists, updating instead",
				log.Str("name", req.Secret.Name),
				log.Str("namespace", types.NS(req.Secret.Namespace)))
			return s.UpdateSecret(ctx, &generated.UpdateSecretRequest{Secret: req.Secret})
		}
		return nil, status.Errorf(codes.Internal, "create secret: %v", err)
	}
	s.logger.Info("Secret created", log.Str("name", req.Secret.Name), log.Str("namespace", req.Secret.Namespace))
	return &generated.SecretResponse{Secret: toProtoSecret(sec), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

func (s *SecretService) GetSecret(ctx context.Context, req *generated.GetSecretRequest) (*generated.SecretResponse, error) {
	if !s.limiter.Allow() {
		return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	sec, err := s.repo.Get(ctx, req.Namespace, req.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "get: %v", err)
	}
	return &generated.SecretResponse{Secret: toProtoSecret(sec), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

func (s *SecretService) UpdateSecret(ctx context.Context, req *generated.UpdateSecretRequest) (*generated.SecretResponse, error) {
	if req.Secret == nil {
		return nil, status.Error(codes.InvalidArgument, "secret is required")
	}
	// Normalize namespace
	namespace := types.NS(req.Secret.Namespace)

	// Fetch existing to decide if an update (version bump) is necessary
	current, err := s.repo.Get(ctx, namespace, req.Secret.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "secret not found: %s/%s", namespace, req.Secret.Name)
	}

	// Build desired secret (without version fields; repo sets it)
	desired := &types.Secret{Name: req.Secret.Name, Namespace: namespace, Type: req.Secret.Type, Data: req.Secret.Data}

	// Compare by type and data content. If unchanged, return current without update
	if current.Type == desired.Type && reflect.DeepEqual(current.Data, desired.Data) {
		s.logger.Info("Secret unchanged; no update",
			log.Str("name", desired.Name), log.Str("namespace", desired.Namespace))
		return &generated.SecretResponse{Secret: toProtoSecret(current), Status: &generated.Status{Code: int32(codes.OK)}}, nil
	}

	// For observability, compute hashes
	oldHash := hashSecret(current)
	newHash := hashSecret(desired)
	s.logger.Info("Updating secret",
		log.Str("name", desired.Name), log.Str("namespace", desired.Namespace),
		log.Str("old_hash", oldHash[:8]), log.Str("new_hash", newHash[:8]))

	// Perform update; repo handles version bump and UpdatedAt
	if err := s.repo.Update(ctx, namespace, req.Secret.Name, desired, store.WithSource(store.EventSourceAPI)); err != nil {
		return nil, status.Errorf(codes.Internal, "update: %v", err)
	}
	got, _ := s.repo.Get(ctx, namespace, req.Secret.Name)
	return &generated.SecretResponse{Secret: toProtoSecret(got), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

func (s *SecretService) DeleteSecret(ctx context.Context, req *generated.DeleteSecretRequest) (*generated.Status, error) {
	if err := s.repo.Delete(ctx, req.Namespace, req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	return &generated.Status{Code: int32(codes.OK)}, nil
}

func (s *SecretService) ListSecrets(ctx context.Context, req *generated.ListSecretsRequest) (*generated.ListSecretsResponse, error) {
	if !s.limiter.Allow() {
		return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	list, err := s.repo.List(ctx, types.NS(req.Namespace))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}
	out := make([]*generated.Secret, 0, len(list))
	for _, sec := range list {
		out = append(out, toProtoSecret(sec))
	}
	return &generated.ListSecretsResponse{Secrets: out, Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// tokenBucket is a lightweight token bucket rate limiter.
type tokenBucket struct {
	mu         sync.Mutex
	capacity   float64
	tokens     float64
	refillRate float64 // tokens per second
	last       time.Time
}

func newTokenBucket(capacity, refillRate float64) *tokenBucket {
	return &tokenBucket{capacity: capacity, tokens: capacity, refillRate: refillRate, last: time.Now()}
}

func (b *tokenBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.refillRate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if b.tokens < 1.0 {
		return false
	}
	b.tokens -= 1.0
	return true
}

func toProtoSecret(s *types.Secret) *generated.Secret {
	return &generated.Secret{
		Name:      s.Name,
		Namespace: s.Namespace,
		Type:      s.Type,
		Data:      s.Data,
		Version:   utils.ToInt32NonNegative(s.Version),
		CreatedAt: s.CreatedAt.Format(time.RFC3339),
		UpdatedAt: s.UpdatedAt.Format(time.RFC3339),
	}
}

// hashSecret returns a deterministic hash for comparing secret content
func hashSecret(s *types.Secret) string {
	// Only hash fields that represent data changes (type + data)
	payload := struct {
		Type string            `json:"type"`
		Data map[string]string `json:"data"`
	}{Type: s.Type, Data: s.Data}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}
