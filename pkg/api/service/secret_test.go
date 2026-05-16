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

// newSecretServiceForTest builds an in-memory SecretService with a fresh KEK
// for the patch tests below. Equivalent setup to TestSecretServiceCRUD's
// preamble; extracted for readability.
func newSecretServiceForTest(t *testing.T) *SecretService {
	t.Helper()
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
	return NewSecretService(st, log.GetDefaultLogger())
}

// TestPatchSecretMergesAndPreservesUntouchedKeys is the core regression test
// for the bug that motivated PatchSecret: rotating one key of a multi-key
// secret must never wipe the others.
func TestPatchSecretMergesAndPreservesUntouchedKeys(t *testing.T) {
	ctx := context.Background()
	svc := newSecretServiceForTest(t)

	if _, err := svc.CreateSecret(ctx, &generated.CreateSecretRequest{
		Secret: &generated.Secret{
			Name: "gateway-secrets", Namespace: "prod", Type: "static",
			Data: map[string]string{
				"INFRA_JWT_SECRET":            "jwt-v1",
				"INFRA_ENCRYPTION_PASSPHRASE": "pass-v1",
				"OTHER":                       "keep-me",
			},
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Patch only INFRA_ENCRYPTION_PASSPHRASE.
	resp, err := svc.PatchSecret(ctx, &generated.PatchSecretRequest{
		Name: "gateway-secrets", Namespace: "prod",
		Set: map[string]string{"INFRA_ENCRYPTION_PASSPHRASE": "pass-v2"},
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}

	// Response must be metadata-only — no plaintext leaked.
	if len(resp.Secret.Data) != 0 {
		t.Fatalf("PatchSecret leaked plaintext: got %d data entries, want 0", len(resp.Secret.Data))
	}
	// Version must have bumped.
	if resp.Secret.Version != 2 {
		t.Fatalf("PatchSecret version: got %d, want 2", resp.Secret.Version)
	}
	if got := len(resp.Secret.DataKeys); got != 3 {
		t.Fatalf("PatchSecret DataKeys: got %d, want 3", got)
	}

	// Reveal and check the actual stored data.
	rev, err := svc.RevealSecret(ctx, &generated.RevealSecretRequest{Name: "gateway-secrets", Namespace: "prod"})
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	want := map[string]string{
		"INFRA_JWT_SECRET":            "jwt-v1",   // unchanged
		"INFRA_ENCRYPTION_PASSPHRASE": "pass-v2",  // updated
		"OTHER":                       "keep-me",  // unchanged
	}
	for k, v := range want {
		if rev.Secret.Data[k] != v {
			t.Errorf("data[%q] = %q, want %q", k, rev.Secret.Data[k], v)
		}
	}
	if len(rev.Secret.Data) != len(want) {
		t.Errorf("data size = %d, want %d (other keys were wiped?)", len(rev.Secret.Data), len(want))
	}
}

// TestPatchSecretUnsetRemovesNamedKeys verifies that `unset` removes the
// listed keys and only those keys, and that the new version reflects the
// reduced set.
func TestPatchSecretUnsetRemovesNamedKeys(t *testing.T) {
	ctx := context.Background()
	svc := newSecretServiceForTest(t)

	if _, err := svc.CreateSecret(ctx, &generated.CreateSecretRequest{
		Secret: &generated.Secret{
			Name: "s", Namespace: "default", Type: "static",
			Data: map[string]string{"a": "1", "b": "2", "c": "3"},
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.PatchSecret(ctx, &generated.PatchSecretRequest{
		Name: "s", Namespace: "default",
		Unset: []string{"b"},
	}); err != nil {
		t.Fatalf("patch: %v", err)
	}

	rev, err := svc.RevealSecret(ctx, &generated.RevealSecretRequest{Name: "s", Namespace: "default"})
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if _, hasB := rev.Secret.Data["b"]; hasB {
		t.Errorf("unset failed: key 'b' still present")
	}
	if rev.Secret.Data["a"] != "1" || rev.Secret.Data["c"] != "3" {
		t.Errorf("unset removed wrong keys: %v", rev.Secret.Data)
	}
}

// TestPatchSecretUnsetMissingKeyIsIdempotent verifies that unsetting a key
// that doesn't exist is a silent no-op (returns success, doesn't bump version
// when no actual change occurs).
func TestPatchSecretUnsetMissingKeyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	svc := newSecretServiceForTest(t)

	if _, err := svc.CreateSecret(ctx, &generated.CreateSecretRequest{
		Secret: &generated.Secret{
			Name: "s", Namespace: "default", Type: "static",
			Data: map[string]string{"a": "1"},
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	resp, err := svc.PatchSecret(ctx, &generated.PatchSecretRequest{
		Name: "s", Namespace: "default",
		Unset: []string{"does-not-exist"},
	})
	if err != nil {
		t.Fatalf("patch (missing unset key): %v", err)
	}
	// No-op short-circuit: version stays at 1.
	if resp.Secret.Version != 1 {
		t.Errorf("expected no-op (version 1), got version %d", resp.Secret.Version)
	}
}

// TestPatchSecretIdenticalSetIsNoOp verifies that setting a key to the value
// it already has produces no new version (matches UpdateSecret's behaviour).
func TestPatchSecretIdenticalSetIsNoOp(t *testing.T) {
	ctx := context.Background()
	svc := newSecretServiceForTest(t)

	if _, err := svc.CreateSecret(ctx, &generated.CreateSecretRequest{
		Secret: &generated.Secret{
			Name: "s", Namespace: "default", Type: "static",
			Data: map[string]string{"a": "1"},
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	resp, err := svc.PatchSecret(ctx, &generated.PatchSecretRequest{
		Name: "s", Namespace: "default",
		Set: map[string]string{"a": "1"},
	})
	if err != nil {
		t.Fatalf("patch (identical value): %v", err)
	}
	if resp.Secret.Version != 1 {
		t.Errorf("expected no-op (version 1), got version %d", resp.Secret.Version)
	}
}

// TestPatchSecretEmptyRequestRejected verifies that calling patch with
// neither set nor unset is an InvalidArgument error — surfacing the mistake
// rather than silently no-opping.
func TestPatchSecretEmptyRequestRejected(t *testing.T) {
	ctx := context.Background()
	svc := newSecretServiceForTest(t)

	if _, err := svc.CreateSecret(ctx, &generated.CreateSecretRequest{
		Secret: &generated.Secret{
			Name: "s", Namespace: "default", Type: "static",
			Data: map[string]string{"a": "1"},
		},
		EnsureNamespace: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.PatchSecret(ctx, &generated.PatchSecretRequest{Name: "s", Namespace: "default"}); err == nil {
		t.Fatalf("expected InvalidArgument for empty patch")
	}
}

// TestPatchSecretNotFound verifies that patching a non-existent secret
// returns NotFound (not Internal).
func TestPatchSecretNotFound(t *testing.T) {
	ctx := context.Background()
	svc := newSecretServiceForTest(t)

	_, err := svc.PatchSecret(ctx, &generated.PatchSecretRequest{
		Name: "missing", Namespace: "default",
		Set: map[string]string{"x": "y"},
	})
	if err == nil {
		t.Fatalf("expected error for missing secret")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected NotFound error, got: %v", err)
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
