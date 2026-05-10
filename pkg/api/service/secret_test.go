package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/crypto"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
)

func TestSecretServiceCRUD(t *testing.T) {
	ctx := context.Background()
	kek, err := crypto.RandomBytes(32)
	if err != nil {
		t.Fatalf("failed to generate KEK: %v", err)
	}
	st := store.NewTestStoreWithOptions(store.StoreOptions{
		KEKBytes:                kek,
		SecretEncryptionEnabled: true,
		SecretLimits: store.Limits{
			MaxObjectBytes:   1 << 20,
			MaxKeyNameLength: 256,
		},
	})

	svc := NewSecretService(st, log.GetDefaultLogger())

	// Create
	createResp, err := svc.CreateSecret(ctx, &generated.CreateSecretRequest{
		Secret: &generated.Secret{
			Name:      "db-credentials",
			Namespace: "prod",
			Type:      "static",
			Data:      map[string]string{"username": "admin", "password": "s3cr3t"},
		},
		EnsureNamespace: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if createResp.Secret == nil || createResp.Secret.Name != "db-credentials" {
		t.Fatalf("bad create resp")
	}

	// Get — as of dev.33 must NOT return plaintext data; must return data_keys
	getResp, err := svc.GetSecret(ctx, &generated.GetSecretRequest{Name: "db-credentials", Namespace: "prod"})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if getResp.Secret.Name != "db-credentials" {
		t.Fatalf("bad get name")
	}
	if len(getResp.Secret.Data) != 0 {
		t.Fatalf("GetSecret leaked plaintext: got %d data entries, want 0", len(getResp.Secret.Data))
	}
	if got := len(getResp.Secret.DataKeys); got != 2 {
		t.Fatalf("GetSecret DataKeys: got %d, want 2", got)
	}

	// List — must also be metadata-only
	listResp, err := svc.ListSecrets(ctx, &generated.ListSecretsRequest{Namespace: "prod"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listResp.Secrets) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(listResp.Secrets))
	}
	if len(listResp.Secrets[0].Data) != 0 {
		t.Fatalf("ListSecrets leaked plaintext")
	}
	if len(listResp.Secrets[0].DataKeys) != 2 {
		t.Fatalf("ListSecrets DataKeys: got %d, want 2", len(listResp.Secrets[0].DataKeys))
	}

	// Reveal — must return plaintext
	revResp, err := svc.RevealSecret(ctx, &generated.RevealSecretRequest{Name: "db-credentials", Namespace: "prod"})
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if revResp.Secret == nil || revResp.Secret.Data["password"] != "s3cr3t" {
		t.Fatalf("RevealSecret did not return plaintext password")
	}

	// Update
	time.Sleep(10 * time.Millisecond)
	updResp, err := svc.UpdateSecret(ctx, &generated.UpdateSecretRequest{Secret: &generated.Secret{
		Name:      "db-credentials",
		Namespace: "prod",
		Type:      "static",
		Data:      map[string]string{"username": "admin", "password": "n3w"},
	}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updResp.Secret == nil {
		t.Fatalf("nil update resp")
	}

	// Delete
	delResp, err := svc.DeleteSecret(ctx, &generated.DeleteSecretRequest{Name: "db-credentials", Namespace: "prod"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if delResp.Code != 0 {
		_ = delResp
	}
}

// TestSecretServiceVersionsAndRollback exercises the dev.34 surface:
// ListSecretVersions returns metadata only, RevealSecretVersion returns
// the plaintext payload of a specific historical version, and RollbackSecret
// rewrites HEAD to a prior version's data while bumping the version counter.
func TestSecretServiceVersionsAndRollback(t *testing.T) {
	ctx := context.Background()
	kek, err := crypto.RandomBytes(32)
	if err != nil {
		t.Fatalf("kek: %v", err)
	}
	st := store.NewTestStoreWithOptions(store.StoreOptions{
		KEKBytes:                kek,
		SecretEncryptionEnabled: true,
		SecretLimits: store.Limits{
			MaxObjectBytes:   1 << 20,
			MaxKeyNameLength: 256,
		},
	})
	svc := NewSecretService(st, log.GetDefaultLogger())

	// Create v1
	if _, err := svc.CreateSecret(ctx, &generated.CreateSecretRequest{
		Secret: &generated.Secret{
			Name:      "api-key",
			Namespace: "prod",
			Type:      "static",
			Data:      map[string]string{"token": "v1-token"},
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("create v1: %v", err)
	}

	// Update -> v2
	time.Sleep(5 * time.Millisecond)
	if _, err := svc.UpdateSecret(ctx, &generated.UpdateSecretRequest{Secret: &generated.Secret{
		Name:      "api-key",
		Namespace: "prod",
		Type:      "static",
		Data:      map[string]string{"token": "v2-token"},
	}}); err != nil {
		t.Fatalf("update v2: %v", err)
	}

	// List versions: expect 2 entries, newest first, no plaintext leaked
	lvResp, err := svc.ListSecretVersions(ctx, &generated.ListSecretVersionsRequest{Name: "api-key", Namespace: "prod"})
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if got := len(lvResp.Versions); got != 2 {
		t.Fatalf("ListSecretVersions: got %d versions, want 2", got)
	}
	if lvResp.Versions[0].Version != 2 || lvResp.Versions[1].Version != 1 {
		t.Fatalf("versions not newest-first: got [%d, %d]", lvResp.Versions[0].Version, lvResp.Versions[1].Version)
	}
	for i, v := range lvResp.Versions {
		if len(v.Data) != 0 {
			t.Fatalf("ListSecretVersions[%d] leaked plaintext", i)
		}
		if len(v.DataKeys) != 1 || v.DataKeys[0] != "token" {
			t.Fatalf("ListSecretVersions[%d] DataKeys: %v", i, v.DataKeys)
		}
	}

	// RevealSecretVersion(1) -> plaintext for v1
	rv1, err := svc.RevealSecretVersion(ctx, &generated.RevealSecretVersionRequest{Name: "api-key", Namespace: "prod", Version: 1})
	if err != nil {
		t.Fatalf("reveal v1: %v", err)
	}
	if rv1.Secret.Data["token"] != "v1-token" {
		t.Fatalf("reveal v1 token = %q, want v1-token", rv1.Secret.Data["token"])
	}

	// Rollback to v1 -> bumps to v3 with v1's payload
	rb, err := svc.RollbackSecret(ctx, &generated.RollbackSecretRequest{Name: "api-key", Namespace: "prod", ToVersion: 1})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rb.Secret.Version != 3 {
		t.Fatalf("rollback bumped to v%d, want v3", rb.Secret.Version)
	}
	// Rollback response is metadata-only; verify by revealing HEAD
	rhead, err := svc.RevealSecret(ctx, &generated.RevealSecretRequest{Name: "api-key", Namespace: "prod"})
	if err != nil {
		t.Fatalf("reveal head: %v", err)
	}
	if rhead.Secret.Version != 3 || rhead.Secret.Data["token"] != "v1-token" {
		t.Fatalf("rollback HEAD: v%d token=%q; want v3 token=v1-token", rhead.Secret.Version, rhead.Secret.Data["token"])
	}

	// Rolling back to current head must fail
	if _, err := svc.RollbackSecret(ctx, &generated.RollbackSecretRequest{Name: "api-key", Namespace: "prod", ToVersion: 3}); err == nil {
		t.Fatalf("expected error rolling back to current HEAD")
	}
}

func TestSecretServiceNoEnsureNamespace(t *testing.T) {
	ctx := context.Background()
	kek, err := crypto.RandomBytes(32)
	if err != nil {
		t.Fatalf("failed to generate KEK: %v", err)
	}
	st := store.NewTestStoreWithOptions(store.StoreOptions{
		KEKBytes:                kek,
		SecretEncryptionEnabled: true,
		SecretLimits: store.Limits{
			MaxObjectBytes:   1 << 20,
			MaxKeyNameLength: 256,
		},
	})
	svc := NewSecretService(st, log.GetDefaultLogger())

	// Try to create secret in non-existent namespace without EnsureNamespace
	_, err = svc.CreateSecret(ctx, &generated.CreateSecretRequest{
		Secret: &generated.Secret{
			Name:      "test-secret",
			Namespace: "non-existent",
			Type:      "static",
			Data:      map[string]string{"key": "value"},
		},
		EnsureNamespace: false,
	})
	if err == nil {
		t.Fatalf("expected error when creating secret in non-existent namespace without EnsureNamespace")
	}

	// Verify the error message indicates namespace doesn't exist
	if !strings.Contains(err.Error(), "namespace") && !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected error about namespace not existing, got: %v", err)
	}
}
