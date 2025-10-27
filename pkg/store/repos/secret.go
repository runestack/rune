package repos

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/crypto"
	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

type SecretRepo struct {
	base *BaseRepo[types.Secret]

	// KEK management for secret repo
	kekOpts *crypto.KEKOptions
	kekOnce sync.Once
	kek     []byte
	kekErr  error

	// secret limits
	secretLimits store.Limits
}

type SecretOption func(*SecretRepo)

func NewSecretRepo(core store.Store, opts ...SecretOption) *SecretRepo {
	repo := &SecretRepo{
		base: NewBaseRepo[types.Secret](core, types.ResourceTypeSecret),
	}

	o := core.GetOpts()
	repo.kekOpts = o.KEKOptions
	repo.secretLimits = o.SecretLimits
	repo.kek = o.KEKBytes

	for _, opt := range opts {
		opt(repo)
	}
	return repo
}

func WithSecretLimits(limits store.Limits) SecretOption {
	return func(r *SecretRepo) {
		r.secretLimits = limits
	}
}

func WithKEKOptions(opts *crypto.KEKOptions) SecretOption {
	return func(r *SecretRepo) {
		r.kekOpts = opts
	}
}

func WithKEKBytes(key []byte) SecretOption {
	return func(r *SecretRepo) {
		r.kek = append([]byte(nil), key...)
	}
}

// Create using fields on secret (no ref required)
func (r *SecretRepo) Create(ctx context.Context, s *types.Secret) error {
	if s == nil || s.Name == "" || s.Namespace == "" {
		return fmt.Errorf("invalid secret")
	}
	if err := utils.ValidateDNS1123Name(s.Name); err != nil {
		return fmt.Errorf("secret name validation failed: %w", err)
	}

	if err := r.validateSecretData(s.Data); err != nil {
		return err
	}

	if s.ID == "" {
		s.ID = uuid.NewString()
	}

	now := time.Now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	rec, err := r.encryptSecret(*s, 1)
	if err != nil {
		return err
	}
	return r.base.core.Create(ctx, types.ResourceTypeSecret, s.Namespace, s.Name, rec)
}

func (r *SecretRepo) CreateRef(ctx context.Context, ref string, s *types.Secret) error {
	pr, err := types.ParseResourceRef(ref)
	if err != nil {
		return err
	}
	if s != nil {
		if s.Namespace == "" {
			s.Namespace = pr.Namespace
		}
		if s.Name == "" {
			s.Name = pr.Name
		}
	}
	return r.Create(ctx, s)
}

func (r *SecretRepo) Get(ctx context.Context, namespace, name string) (*types.Secret, error) {
	var stored types.StoredSecret
	if err := r.base.core.Get(ctx, types.ResourceTypeSecret, namespace, name, &stored); err != nil {
		return nil, err
	}
	return r.decryptSecret(stored)
}

func (r *SecretRepo) Update(ctx context.Context, namespace, name string, s *types.Secret, opts ...store.UpdateOption) error {
	cur, err := r.Get(ctx, namespace, name)
	if err != nil {
		return err
	}
	next := cur.Version + 1
	if err := utils.ValidateDNS1123Name(s.Name); err != nil {
		return fmt.Errorf("secret name validation failed: %w", err)
	}
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	if err := r.validateSecretData(s.Data); err != nil {
		return err
	}
	s.CreatedAt = cur.CreatedAt
	s.UpdatedAt = time.Now()
	rec, err := r.encryptSecret(*s, next)
	if err != nil {
		return err
	}
	return r.base.core.Update(ctx, types.ResourceTypeSecret, namespace, name, rec, opts...)
}

func (r *SecretRepo) Delete(ctx context.Context, namespace, name string) error {
	return r.base.core.Delete(ctx, types.ResourceTypeSecret, namespace, name)
}

func (r *SecretRepo) List(ctx context.Context, namespace string) ([]*types.Secret, error) {
	var storedList []types.StoredSecret
	if err := r.base.core.List(ctx, types.ResourceTypeSecret, namespace, &storedList); err != nil {
		return nil, err
	}
	out := make([]*types.Secret, 0, len(storedList))
	for _, rec := range storedList {
		s, err := r.decryptSecret(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (r *SecretRepo) encryptSecret(s types.Secret, version int) (types.StoredSecret, error) {
	pt, err := json.Marshal(s.Data)
	if err != nil {
		return types.StoredSecret{}, err
	}
	dek, err := crypto.RandomBytes(32)
	if err != nil {
		return types.StoredSecret{}, err
	}
	aead, err := crypto.NewAEADCipher(dek)
	if err != nil {
		return types.StoredSecret{}, err
	}
	ad := []byte(fmt.Sprintf("%s|%s|%d", s.Namespace, s.Name, version))
	ct, err := aead.Encrypt(pt, ad)
	if err != nil {
		return types.StoredSecret{}, err
	}
	kek, err := r.ensureKEK()
	if err != nil {
		return types.StoredSecret{}, err
	}
	kekAEAD, err := crypto.NewAEADCipher(kek)
	if err != nil {
		return types.StoredSecret{}, err
	}
	wrapped, err := kekAEAD.Encrypt(dek, []byte(fmt.Sprintf("%s|%s|%d|dek", s.Namespace, s.Name, version)))
	if err != nil {
		return types.StoredSecret{}, err
	}
	return types.StoredSecret{
		Name:       s.Name,
		Namespace:  s.Namespace,
		Version:    version,
		CreatedAt:  s.CreatedAt,
		UpdatedAt:  s.UpdatedAt,
		Ciphertext: ct,
		WrappedDEK: wrapped,
	}, nil
}

func (r *SecretRepo) decryptSecret(rec types.StoredSecret) (*types.Secret, error) {
	kek, err := r.ensureKEK()
	if err != nil {
		return nil, err
	}
	kekAEAD, err := crypto.NewAEADCipher(kek)
	if err != nil {
		return nil, err
	}
	dek, err := kekAEAD.Decrypt(rec.WrappedDEK, []byte(fmt.Sprintf("%s|%s|%d|dek", rec.Namespace, rec.Name, rec.Version)))
	if err != nil {
		return nil, err
	}
	aead, err := crypto.NewAEADCipher(dek)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Decrypt(rec.Ciphertext, []byte(fmt.Sprintf("%s|%s|%d", rec.Namespace, rec.Name, rec.Version)))
	if err != nil {
		return nil, err
	}
	var data map[string]string
	if err := json.Unmarshal(pt, &data); err != nil {
		return nil, err
	}
	return &types.Secret{
		Name:      rec.Name,
		Namespace: rec.Namespace,
		Version:   rec.Version,
		Data:      data,
		CreatedAt: rec.CreatedAt,
		UpdatedAt: rec.UpdatedAt,
		Type:      "static",
	}, nil
}

// ensureKEK loads or generates a KEK for secret repo
func (r *SecretRepo) ensureKEK() ([]byte, error) {
	r.kekOnce.Do(func() {
		if len(r.kek) == 32 {
			// already injected via WithKEKBytes
			return
		}
		if r.kekOpts == nil {
			r.kekErr = fmt.Errorf("KEK options not configured in BaseRepo; use WithKEKOptions or WithKEKBytes")
			return
		}
		r.kek, r.kekErr = crypto.LoadOrGenerateKEK(*r.kekOpts)
	})
	return r.kek, r.kekErr
}

func (r *SecretRepo) validateSecretData(data map[string]string) error {
	var total int
	for k, v := range data {
		if len(k) > r.secretLimits.MaxKeyNameLength {
			return fmt.Errorf("secret key name too long: %s", k)
		}
		total += len(v)
		if total > r.secretLimits.MaxObjectBytes {
			return fmt.Errorf("secret data exceeds 1MiB limit")
		}
	}
	return nil
}
