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
