package registryauth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGCPProviderDefaultHostMatching(t *testing.T) {
	p := NewGCPProvider(GCPConfig{})
	for _, host := range []string{
		"europe-west2-docker.pkg.dev",
		"us-central1-docker.pkg.dev",
		"gcr.io",
		"us.gcr.io",
		"eu.gcr.io",
	} {
		if !p.Match(host, host+"/proj/app:v1") {
			t.Errorf("expected default GCP provider to match %s", host)
		}
	}
	for _, host := range []string{
		"ghcr.io",
		"index.docker.io",
		"notgcr.io.evil.com",
		"pkg.dev", // bare apex is not a registry host
	} {
		if p.Match(host, host+"/proj/app:v1") {
			t.Errorf("expected default GCP provider NOT to match %s", host)
		}
	}
}

func TestGCPProviderExplicitPattern(t *testing.T) {
	p := NewGCPProvider(GCPConfig{Registry: "*.pkg.dev"})
	if !p.Match("europe-west2-docker.pkg.dev", "europe-west2-docker.pkg.dev/proj/app:v1") {
		t.Error("expected pattern *.pkg.dev to match AR host")
	}
	if p.Match("gcr.io", "gcr.io/proj/app:v1") {
		t.Error("explicit pattern should override default host set")
	}
}

func TestGCPProviderResolveEncodesOAuthToken(t *testing.T) {
	p := NewGCPProvider(GCPConfig{})
	p.fetchToken = func(ctx context.Context) (string, time.Time, error) {
		return "ya29.token", time.Now().Add(time.Hour), nil
	}
	b64, err := p.Resolve(context.Background(), "europe-west2-docker.pkg.dev", "europe-west2-docker.pkg.dev/proj/repo/app:1.0")
	if err != nil {
		t.Fatal(err)
	}
	m, err := decode(b64)
	if err != nil {
		t.Fatal(err)
	}
	if m["username"] != "oauth2accesstoken" || m["password"] != "ya29.token" {
		t.Fatalf("unexpected creds: %+v", m)
	}
	if m["serveraddress"] != "europe-west2-docker.pkg.dev" {
		t.Fatalf("unexpected serveraddress: %s", m["serveraddress"])
	}
}

func TestGCPProviderCachesUntilExpiry(t *testing.T) {
	calls := 0
	p := NewGCPProvider(GCPConfig{})
	p.fetchToken = func(ctx context.Context) (string, time.Time, error) {
		calls++
		return "tok", time.Now().Add(time.Hour), nil
	}
	for i := 0; i < 3; i++ {
		if _, err := p.Resolve(context.Background(), "gcr.io", "gcr.io/proj/app"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("expected 1 metadata fetch, got %d", calls)
	}
}

func TestGCPProviderRefetchesNearExpiry(t *testing.T) {
	calls := 0
	p := NewGCPProvider(GCPConfig{})
	p.fetchToken = func(ctx context.Context) (string, time.Time, error) {
		calls++
		// within the 5-minute refresh margin → next Resolve re-fetches
		return "tok", time.Now().Add(time.Minute), nil
	}
	for i := 0; i < 2; i++ {
		if _, err := p.Resolve(context.Background(), "gcr.io", "gcr.io/proj/app"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("expected 2 metadata fetches, got %d", calls)
	}
}

func TestGCPProviderFallsBackToAnonymousOnError(t *testing.T) {
	p := NewGCPProvider(GCPConfig{})
	p.fetchToken = func(ctx context.Context) (string, time.Time, error) {
		return "", time.Time{}, errors.New("metadata unreachable")
	}
	b64, err := p.Resolve(context.Background(), "gcr.io", "gcr.io/proj/app")
	if err != nil {
		t.Fatalf("expected nil error (anonymous fallback), got %v", err)
	}
	if b64 != "" {
		t.Fatalf("expected empty auth on fetch failure, got %q", b64)
	}
}

func TestFactoryBuildsGCPProvider(t *testing.T) {
	regs := []map[string]any{
		{"registry": "*.pkg.dev", "auth": map[string]any{"type": "gcp"}},
	}
	ps := BuildProviders(context.Background(), regs)
	if len(ps) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(ps))
	}
	if _, ok := ps[0].(*GCPProvider); !ok {
		t.Fatalf("expected *GCPProvider, got %T", ps[0])
	}
}
