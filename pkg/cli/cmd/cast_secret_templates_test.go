package cmd

import (
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/types"
)

func makeSecret(ns, name string, data map[string]string) *types.Secret {
	if ns == "" {
		ns = "default"
	}
	cp := make(map[string]string, len(data))
	for k, v := range data {
		cp[k] = v
	}
	return &types.Secret{
		Namespace: ns,
		Name:      name,
		Type:      "static",
		Data:      cp,
	}
}

func newInfoFromSecrets(secrets ...*types.Secret) *ResourceInfo {
	return &ResourceInfo{
		FilesByType:      map[string][]string{},
		ServicesByFile:   map[string][]*types.Service{},
		SecretsByFile:    map[string][]*types.Secret{"test.yaml": secrets},
		ConfigmapsByFile: map[string][]*types.Configmap{},
		TotalResources:   len(secrets),
	}
}

func TestRenderSecretTemplates_NoTemplates(t *testing.T) {
	t.Parallel()
	s := makeSecret("default", "plain", map[string]string{"value": "hello"})
	info := newInfoFromSecrets(s)
	if err := renderSecretTemplates(nil, info); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Data["value"] != "hello" {
		t.Fatalf("expected unchanged value, got %q", s.Data["value"])
	}
}

func TestRenderSecretTemplates_SameNamespaceComponents(t *testing.T) {
	t.Parallel()
	host := makeSecret("default", "db-host", map[string]string{"value": "postgres.internal"})
	pass := makeSecret("default", "db-password", map[string]string{"value": "s3cr3t"})
	creds := makeSecret("default", "db-credentials", map[string]string{
		"DATABASE_URL": "postgres://u:{{ secret:db-password/value }}@{{ secret:db-host/value }}:5432/api",
	})

	info := newInfoFromSecrets(host, pass, creds)
	if err := renderSecretTemplates(nil, info); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := creds.Data["DATABASE_URL"]
	want := "postgres://u:s3cr3t@postgres.internal:5432/api"
	if got != want {
		t.Fatalf("rendered value mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderSecretTemplates_FQDNCrossNamespace(t *testing.T) {
	t.Parallel()
	host := makeSecret("infra", "db-host", map[string]string{"value": "db.infra.local"})
	creds := makeSecret("app", "db-credentials", map[string]string{
		"URL": "{{ secret:db-host.infra.rune/value }}",
	})
	info := newInfoFromSecrets(host, creds)
	if err := renderSecretTemplates(nil, info); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.Data["URL"] != "db.infra.local" {
		t.Fatalf("cross-ns render mismatch: %q", creds.Data["URL"])
	}
}

func TestRenderSecretTemplates_MissingComponentNoAPIClient(t *testing.T) {
	t.Parallel()
	creds := makeSecret("default", "creds", map[string]string{
		"V": "{{ secret:nonexistent/value }}",
	})
	info := newInfoFromSecrets(creds)
	err := renderSecretTemplates(nil, info)
	if err == nil {
		t.Fatalf("expected error for missing component, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("error should name missing component, got: %v", err)
	}
}

func TestRenderSecretTemplates_MissingKey(t *testing.T) {
	t.Parallel()
	host := makeSecret("default", "db-host", map[string]string{"value": "x"})
	creds := makeSecret("default", "creds", map[string]string{
		"V": "{{ secret:db-host/wrongkey }}",
	})
	info := newInfoFromSecrets(host, creds)
	err := renderSecretTemplates(nil, info)
	if err == nil {
		t.Fatalf("expected error for missing key")
	}
	if !strings.Contains(err.Error(), "wrongkey") {
		t.Fatalf("error should name missing key, got: %v", err)
	}
}

func TestRenderSecretTemplates_Cycle(t *testing.T) {
	t.Parallel()
	a := makeSecret("default", "a", map[string]string{"v": "{{ secret:b/v }}"})
	b := makeSecret("default", "b", map[string]string{"v": "{{ secret:a/v }}"})
	info := newInfoFromSecrets(a, b)
	err := renderSecretTemplates(nil, info)
	if err == nil {
		t.Fatalf("expected cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error should mention cycle, got: %v", err)
	}
}

func TestRenderSecretTemplates_SelfReference(t *testing.T) {
	t.Parallel()
	a := makeSecret("default", "a", map[string]string{
		"x": "y",
		"z": "{{ secret:a/x }}",
	})
	info := newInfoFromSecrets(a)
	err := renderSecretTemplates(nil, info)
	if err == nil {
		t.Fatalf("expected self-reference error")
	}
	if !strings.Contains(err.Error(), "itself") {
		t.Fatalf("error should mention self-reference, got: %v", err)
	}
}

func TestRenderSecretTemplates_InvalidRefMissingKey(t *testing.T) {
	t.Parallel()
	a := makeSecret("default", "a", map[string]string{"v": "{{ secret:db-host }}"})
	info := newInfoFromSecrets(a)
	err := renderSecretTemplates(nil, info)
	if err == nil {
		t.Fatalf("expected error for missing /<key> segment")
	}
	if !strings.Contains(err.Error(), "missing /<key>") {
		t.Fatalf("error should explain missing key segment, got: %v", err)
	}
}

func TestRenderSecretTemplates_MultipleRefsInOneValue(t *testing.T) {
	t.Parallel()
	a := makeSecret("default", "a", map[string]string{"value": "alpha"})
	b := makeSecret("default", "b", map[string]string{"value": "beta"})
	c := makeSecret("default", "c", map[string]string{
		"combined": "{{ secret:a/value }}-{{ secret:b/value }}-{{ secret:a/value }}",
	})
	info := newInfoFromSecrets(a, b, c)
	if err := renderSecretTemplates(nil, info); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := c.Data["combined"], "alpha-beta-alpha"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderSecretTemplates_TopoOrderTransitive(t *testing.T) {
	t.Parallel()
	// a is a leaf; b depends on a; c depends on b.
	// Order of rendering must be a, b, c so that c sees b's rendered value.
	a := makeSecret("default", "a", map[string]string{"v": "leaf"})
	b := makeSecret("default", "b", map[string]string{"v": "B-{{ secret:a/v }}"})
	c := makeSecret("default", "c", map[string]string{"v": "C-{{ secret:b/v }}"})
	info := newInfoFromSecrets(c, b, a) // intentionally out of order
	if err := renderSecretTemplates(nil, info); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := b.Data["v"], "B-leaf"; got != want {
		t.Fatalf("b got %q want %q", got, want)
	}
	if got, want := c.Data["v"], "C-B-leaf"; got != want {
		t.Fatalf("c got %q want %q", got, want)
	}
}

func TestRenderSecretTemplates_DefaultsNamespace(t *testing.T) {
	t.Parallel()
	// Secret created without an explicit namespace should be normalized to
	// "default" and resolve same-namespace refs accordingly.
	host := &types.Secret{Name: "db-host", Type: "static", Data: map[string]string{"value": "h"}}
	creds := &types.Secret{Name: "creds", Type: "static", Data: map[string]string{
		"v": "{{ secret:db-host/value }}",
	}}
	info := newInfoFromSecrets(host, creds)
	if err := renderSecretTemplates(nil, info); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.Data["v"] != "h" {
		t.Fatalf("expected default-namespace lookup to succeed, got %q", creds.Data["v"])
	}
}
