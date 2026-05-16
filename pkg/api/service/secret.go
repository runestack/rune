package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
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

// SecretService implements the gRPC SecretService.
//
// As of dev.33, the read-side surface is split:
//   - GetSecret / ListSecrets return metadata only (no plaintext payload).
//     They populate Secret.data_keys instead of data.
//   - RevealSecret returns the plaintext payload and requires the
//     secrets:reveal RBAC verb.
//
// Every Create / Update / Delete / Reveal / Get call emits an AuditEvent so
// that operators have a queryable trail separate from the structured app log.
type SecretService struct {
	generated.UnimplementedSecretServiceServer
	repo   *repos.SecretRepo
	nsRepo *repos.NamespaceRepo
	audit  *repos.AuditRepo
	logger log.Logger

	limiter *tokenBucket

	// patchLocks serialises concurrent PatchSecret calls per namespace/name
	// so the read-merge-write cycle is atomic for a given secret. Without
	// this, two patches setting different keys could race and the second
	// writer would clobber the first's change. Acquired only on the patch
	// path; replace-style UpdateSecret is unaffected.
	patchLocks sync.Map // key: "namespace/name" → *sync.Mutex
}

func NewSecretService(coreStore store.Store, logger log.Logger) *SecretService {
	return &SecretService{
		repo:    repos.NewSecretRepo(coreStore),
		nsRepo:  repos.NewNamespaceRepo(coreStore),
		audit:   repos.NewAuditRepo(coreStore),
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
		s.emitAudit(ctx, "create", sec.Namespace, sec.Name, types.AuditOutcomeError, err.Error(), nil)
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
		s.emitAudit(ctx, "create", sec.Namespace, sec.Name, types.AuditOutcomeError, err.Error(), nil)
		return nil, status.Errorf(codes.Internal, "create secret: %v", err)
	}
	s.logger.Info("Secret created", log.Str("name", req.Secret.Name), log.Str("namespace", req.Secret.Namespace))
	s.emitAudit(ctx, "create", sec.Namespace, sec.Name, types.AuditOutcomeSuccess, "", map[string]string{
		"version":  fmt.Sprintf("%d", sec.Version),
		"keyCount": fmt.Sprintf("%d", len(sec.Data)),
	})
	return &generated.SecretResponse{Secret: toProtoSecret(sec, false), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

func (s *SecretService) GetSecret(ctx context.Context, req *generated.GetSecretRequest) (*generated.SecretResponse, error) {
	if !s.limiter.Allow() {
		return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	sec, err := s.repo.Get(ctx, req.Namespace, req.Name)
	if err != nil {
		s.emitAudit(ctx, "get", types.NS(req.Namespace), req.Name, types.AuditOutcomeError, err.Error(), nil)
		return nil, status.Errorf(codes.NotFound, "get: %v", err)
	}
	s.emitAudit(ctx, "get", sec.Namespace, sec.Name, types.AuditOutcomeSuccess, "", nil)
	return &generated.SecretResponse{Secret: toProtoSecret(sec, false), Status: &generated.Status{Code: int32(codes.OK)}}, nil
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
		s.emitAudit(ctx, "update", namespace, req.Secret.Name, types.AuditOutcomeError, "secret not found", nil)
		return nil, status.Errorf(codes.NotFound, "secret not found: %s/%s", namespace, req.Secret.Name)
	}

	// Build desired secret (without version fields; repo sets it)
	desired := &types.Secret{Name: req.Secret.Name, Namespace: namespace, Type: req.Secret.Type, Data: req.Secret.Data}

	// Compare by type and data content. If unchanged, return current without update
	if current.Type == desired.Type && reflect.DeepEqual(current.Data, desired.Data) {
		s.logger.Info("Secret unchanged; no update",
			log.Str("name", desired.Name), log.Str("namespace", desired.Namespace))
		return &generated.SecretResponse{Secret: toProtoSecret(current, false), Status: &generated.Status{Code: int32(codes.OK)}}, nil
	}

	// For observability, compute hashes
	oldHash := hashSecret(current)
	newHash := hashSecret(desired)
	s.logger.Info("Updating secret",
		log.Str("name", desired.Name), log.Str("namespace", desired.Namespace),
		log.Str("old_hash", oldHash[:8]), log.Str("new_hash", newHash[:8]))

	// Perform update; repo handles version bump and UpdatedAt
	if err := s.repo.Update(ctx, namespace, req.Secret.Name, desired, store.WithSource(store.EventSourceAPI)); err != nil {
		s.emitAudit(ctx, "update", namespace, req.Secret.Name, types.AuditOutcomeError, err.Error(), nil)
		return nil, status.Errorf(codes.Internal, "update: %v", err)
	}
	got, _ := s.repo.Get(ctx, namespace, req.Secret.Name)
	meta := map[string]string{
		"oldHash":  oldHash[:8],
		"newHash":  newHash[:8],
		"keyCount": fmt.Sprintf("%d", len(desired.Data)),
	}
	if got != nil {
		meta["version"] = fmt.Sprintf("%d", got.Version)
	}
	s.emitAudit(ctx, "update", namespace, req.Secret.Name, types.AuditOutcomeSuccess, "", meta)
	return &generated.SecretResponse{Secret: toProtoSecret(got, false), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// PatchSecret applies a key-scoped merge to an existing secret: `set` entries
// upsert, `unset` keys are removed. Runs under a per-secret lock so concurrent
// patches don't clobber each other. The response carries metadata only — no
// plaintext — so callers don't need secrets:reveal to rotate a single key.
//
// Idempotency:
//   - If neither set nor unset is provided, returns InvalidArgument.
//   - unset keys that don't exist are silently ignored.
//   - If the merge produces a map identical to the current head, no new
//     version is written and no audit event is emitted (matches UpdateSecret's
//     unchanged-short-circuit behaviour).
//
// Audit:
//   - On success, action="patch" with metadata listing the key names that
//     were set/unset and the resulting keyCount/version. Values are never
//     logged.
func (s *SecretService) PatchSecret(ctx context.Context, req *generated.PatchSecretRequest) (*generated.SecretResponse, error) {
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if len(req.Set) == 0 && len(req.Unset) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one of set or unset must be non-empty")
	}

	namespace := types.NS(req.Namespace)

	// Serialise concurrent patches on this secret. Single-writer per key keeps
	// the read-merge-write atomic without touching the store layer.
	mu := s.acquirePatchLock(namespace, req.Name)
	mu.Lock()
	defer mu.Unlock()

	current, err := s.repo.Get(ctx, namespace, req.Name)
	if err != nil {
		s.emitAudit(ctx, "patch", namespace, req.Name, types.AuditOutcomeError, "secret not found", nil)
		return nil, status.Errorf(codes.NotFound, "secret not found: %s/%s", namespace, req.Name)
	}

	// Build the merged data map. Start from current, apply set, then unset.
	merged := make(map[string]string, len(current.Data)+len(req.Set))
	for k, v := range current.Data {
		merged[k] = v
	}
	for k, v := range req.Set {
		merged[k] = v
	}
	removed := make([]string, 0, len(req.Unset))
	for _, k := range req.Unset {
		if _, ok := merged[k]; ok {
			delete(merged, k)
			removed = append(removed, k)
		}
	}

	desired := &types.Secret{Name: req.Name, Namespace: namespace, Type: current.Type, Data: merged}

	// Short-circuit no-op patches (matches UpdateSecret's unchanged path).
	if reflect.DeepEqual(current.Data, desired.Data) {
		s.logger.Info("Secret patch is a no-op; no update",
			log.Str("name", desired.Name), log.Str("namespace", desired.Namespace))
		return &generated.SecretResponse{Secret: toProtoSecret(current, false), Status: &generated.Status{Code: int32(codes.OK)}}, nil
	}

	oldHash := hashSecret(current)
	newHash := hashSecret(desired)
	s.logger.Info("Patching secret",
		log.Str("name", desired.Name), log.Str("namespace", desired.Namespace),
		log.Str("old_hash", oldHash[:8]), log.Str("new_hash", newHash[:8]),
		log.Int("set", len(req.Set)), log.Int("unset", len(removed)))

	if err := s.repo.Update(ctx, namespace, req.Name, desired, store.WithSource(store.EventSourceAPI)); err != nil {
		s.emitAudit(ctx, "patch", namespace, req.Name, types.AuditOutcomeError, err.Error(), nil)
		return nil, status.Errorf(codes.Internal, "patch: %v", err)
	}
	got, _ := s.repo.Get(ctx, namespace, req.Name)

	// Audit metadata carries key NAMES only — never values.
	setKeys := make([]string, 0, len(req.Set))
	for k := range req.Set {
		setKeys = append(setKeys, k)
	}
	sort.Strings(setKeys)
	sort.Strings(removed)
	meta := map[string]string{
		"oldHash":  oldHash[:8],
		"newHash":  newHash[:8],
		"keyCount": fmt.Sprintf("%d", len(desired.Data)),
		"set":      strings.Join(setKeys, ","),
		"unset":    strings.Join(removed, ","),
	}
	if got != nil {
		meta["version"] = fmt.Sprintf("%d", got.Version)
	}
	s.emitAudit(ctx, "patch", namespace, req.Name, types.AuditOutcomeSuccess, "", meta)
	return &generated.SecretResponse{Secret: toProtoSecret(got, false), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// acquirePatchLock returns the per-secret mutex, lazily creating it on first
// use. Stored in a sync.Map so we don't need a global lock around the map
// itself; LoadOrStore handles the race between two first-time patchers.
func (s *SecretService) acquirePatchLock(namespace, name string) *sync.Mutex {
	key := namespace + "/" + name
	mu, _ := s.patchLocks.LoadOrStore(key, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func (s *SecretService) DeleteSecret(ctx context.Context, req *generated.DeleteSecretRequest) (*generated.Status, error) {
	if err := s.repo.Delete(ctx, req.Namespace, req.Name); err != nil {
		s.emitAudit(ctx, "delete", types.NS(req.Namespace), req.Name, types.AuditOutcomeError, err.Error(), nil)
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	s.emitAudit(ctx, "delete", types.NS(req.Namespace), req.Name, types.AuditOutcomeSuccess, "", nil)
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
		out = append(out, toProtoSecret(sec, false))
	}
	return &generated.ListSecretsResponse{Secrets: out, Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// RevealSecret returns the plaintext payload of a single secret. It is rate
// limited identically to GetSecret and emits a `reveal` audit event on every
// call (success or denial).
func (s *SecretService) RevealSecret(ctx context.Context, req *generated.RevealSecretRequest) (*generated.SecretResponse, error) {
	if !s.limiter.Allow() {
		return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	sec, err := s.repo.Get(ctx, req.Namespace, req.Name)
	if err != nil {
		s.emitAudit(ctx, "reveal", types.NS(req.Namespace), req.Name, types.AuditOutcomeError, err.Error(), nil)
		return nil, status.Errorf(codes.NotFound, "reveal: %v", err)
	}
	s.emitAudit(ctx, "reveal", sec.Namespace, sec.Name, types.AuditOutcomeSuccess, "", map[string]string{
		"version":  fmt.Sprintf("%d", sec.Version),
		"keyCount": fmt.Sprintf("%d", len(sec.Data)),
	})
	return &generated.SecretResponse{Secret: toProtoSecret(sec, true), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// ListSecretVersions returns metadata for every historical version of a
// secret, newest first. Plaintext data is stripped; data_keys is populated.
// Use RevealSecretVersion to fetch the payload of a specific version.
func (s *SecretService) ListSecretVersions(ctx context.Context, req *generated.ListSecretVersionsRequest) (*generated.ListSecretVersionsResponse, error) {
	if !s.limiter.Allow() {
		return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	versions, err := s.repo.ListVersions(ctx, req.Namespace, req.Name)
	if err != nil {
		s.emitAudit(ctx, "list-versions", types.NS(req.Namespace), req.Name, types.AuditOutcomeError, err.Error(), nil)
		return nil, status.Errorf(codes.NotFound, "list versions: %v", err)
	}
	out := make([]*generated.Secret, 0, len(versions))
	for _, v := range versions {
		out = append(out, toProtoSecret(v, false))
	}
	s.emitAudit(ctx, "list-versions", types.NS(req.Namespace), req.Name, types.AuditOutcomeSuccess, "", map[string]string{
		"count": fmt.Sprintf("%d", len(versions)),
	})
	return &generated.ListSecretVersionsResponse{Versions: out, Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// RevealSecretVersion returns the plaintext payload of a specific historical
// version of a secret. Gated by secrets:reveal.
func (s *SecretService) RevealSecretVersion(ctx context.Context, req *generated.RevealSecretVersionRequest) (*generated.SecretResponse, error) {
	if !s.limiter.Allow() {
		return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.Version <= 0 {
		return nil, status.Error(codes.InvalidArgument, "version must be > 0")
	}
	sec, err := s.repo.GetVersionN(ctx, req.Namespace, req.Name, int(req.Version))
	if err != nil {
		s.emitAudit(ctx, "reveal-version", types.NS(req.Namespace), req.Name, types.AuditOutcomeError, err.Error(), map[string]string{
			"version": fmt.Sprintf("%d", req.Version),
		})
		return nil, status.Errorf(codes.NotFound, "reveal version: %v", err)
	}
	s.emitAudit(ctx, "reveal-version", sec.Namespace, sec.Name, types.AuditOutcomeSuccess, "", map[string]string{
		"version":  fmt.Sprintf("%d", sec.Version),
		"keyCount": fmt.Sprintf("%d", len(sec.Data)),
	})
	return &generated.SecretResponse{Secret: toProtoSecret(sec, true), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// RollbackSecret rewrites HEAD to the contents of the given prior version,
// producing a new HEAD version. Old versions are retained in history. Gated
// by secrets:update; emits a `rollback` audit event whose metadata records
// fromVersion (previous head), toVersion (rollback target), and newVersion
// (the freshly written head).
func (s *SecretService) RollbackSecret(ctx context.Context, req *generated.RollbackSecretRequest) (*generated.SecretResponse, error) {
	if req == nil || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.ToVersion <= 0 {
		return nil, status.Error(codes.InvalidArgument, "to_version must be > 0")
	}
	cur, err := s.repo.Get(ctx, req.Namespace, req.Name)
	if err != nil {
		s.emitAudit(ctx, "rollback", types.NS(req.Namespace), req.Name, types.AuditOutcomeError, err.Error(), nil)
		return nil, status.Errorf(codes.NotFound, "rollback: %v", err)
	}
	if int(req.ToVersion) == cur.Version {
		s.emitAudit(ctx, "rollback", cur.Namespace, cur.Name, types.AuditOutcomeError, "rollback target equals current head", map[string]string{
			"toVersion": fmt.Sprintf("%d", req.ToVersion),
		})
		return nil, status.Errorf(codes.FailedPrecondition, "version %d is already HEAD", req.ToVersion)
	}
	rolled, err := s.repo.Rollback(ctx, req.Namespace, req.Name, int(req.ToVersion), store.WithSource(store.EventSourceAPI))
	if err != nil {
		s.emitAudit(ctx, "rollback", types.NS(req.Namespace), req.Name, types.AuditOutcomeError, err.Error(), map[string]string{
			"toVersion": fmt.Sprintf("%d", req.ToVersion),
		})
		return nil, status.Errorf(codes.Internal, "rollback: %v", err)
	}
	s.emitAudit(ctx, "rollback", rolled.Namespace, rolled.Name, types.AuditOutcomeSuccess, "", map[string]string{
		"fromVersion": fmt.Sprintf("%d", cur.Version),
		"toVersion":   fmt.Sprintf("%d", req.ToVersion),
		"newVersion":  fmt.Sprintf("%d", rolled.Version),
	})
	return &generated.SecretResponse{Secret: toProtoSecret(rolled, false), Status: &generated.Status{Code: int32(codes.OK)}}, nil
}

// emitAudit writes a single audit event for a secret operation. Failures to
// write are logged but never propagated to the caller — losing an audit row
// must not break the underlying RPC.
func (s *SecretService) emitAudit(ctx context.Context, action, namespace, name string, outcome types.AuditOutcome, msg string, meta map[string]string) {
	if s.audit == nil {
		return
	}
	evt := &types.AuditEvent{
		Actor:       actorFromContext(ctx),
		Action:      action,
		Resource:    "secrets",
		ResourceRef: repos.MakeAuditRef(namespace, name),
		Namespace:   namespace,
		Outcome:     outcome,
		Message:     msg,
		Metadata:    meta,
	}
	if err := s.audit.Append(ctx, evt); err != nil {
		s.logger.Warn("failed to append audit event",
			log.Str("action", action),
			log.Str("ref", evt.ResourceRef),
			log.Err(err))
	}
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

// toProtoSecret converts a typed Secret to its proto representation.
//
// When reveal is false, the plaintext data map is stripped and replaced with
// data_keys so callers can still see which keys exist without seeing values.
// When reveal is true, the plaintext payload is included verbatim and
// data_keys is also populated for caller convenience.
func toProtoSecret(s *types.Secret, reveal bool) *generated.Secret {
	if s == nil {
		return nil
	}
	keys := make([]string, 0, len(s.Data))
	for k := range s.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := &generated.Secret{
		Name:      s.Name,
		Namespace: s.Namespace,
		Type:      s.Type,
		Version:   utils.ToInt32NonNegative(s.Version),
		CreatedAt: s.CreatedAt.Format(time.RFC3339),
		UpdatedAt: s.UpdatedAt.Format(time.RFC3339),
		DataKeys:  keys,
	}
	if reveal {
		out.Data = s.Data
	}
	return out
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
