package registryauth

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
)

// GCPConfig configures a GCPProvider. Registry is an optional host
// pattern (e.g. "*.pkg.dev"); when empty the provider matches the
// standard Google registry hosts (*.pkg.dev, gcr.io, *.gcr.io).
type GCPConfig struct {
	Registry string
}

// GCPProvider resolves Artifact Registry / Container Registry
// credentials from the GCE metadata server: the instance service
// account's access token is used as the password for the
// "oauth2accesstoken" user — the documented Docker auth scheme for
// Google registries. This is what makes the Terraform module's
// enable_artifact_registry_access flag (roles/artifactregistry.reader
// on the instance SA) actually work for private pulls (issue #144).
//
// Mirrors ECRProvider: the token is cached until shortly before
// expiry, and any fetch failure falls back to anonymous pulls.
type GCPProvider struct {
	cfg GCPConfig

	mu      sync.Mutex
	token   string
	expires time.Time

	// fetchToken is swapped out in tests; defaults to the GCE
	// metadata server.
	fetchToken func(ctx context.Context) (string, time.Time, error)
}

func NewGCPProvider(cfg GCPConfig) *GCPProvider {
	return &GCPProvider{cfg: cfg, fetchToken: fetchGCEMetadataToken}
}

func (p *GCPProvider) Match(host string) bool {
	if p.cfg.Registry != "" {
		return hostMatches(p.cfg.Registry, host)
	}
	return isGoogleRegistryHost(host)
}

func isGoogleRegistryHost(host string) bool {
	h := strings.ToLower(host)
	return strings.HasSuffix(h, ".pkg.dev") || h == "gcr.io" || strings.HasSuffix(h, ".gcr.io")
}

func (p *GCPProvider) Resolve(ctx context.Context, host, imageRef string) (string, error) {
	p.mu.Lock()
	if p.token != "" && time.Until(p.expires) > 5*time.Minute {
		tok := p.token
		p.mu.Unlock()
		return encode("oauth2accesstoken", tok, host), nil
	}
	p.mu.Unlock()

	tok, exp, err := p.fetchToken(ctx)
	if err != nil || tok == "" {
		return "", nil // fall back to anonymous, matching ECRProvider
	}
	p.mu.Lock()
	p.token, p.expires = tok, exp
	p.mu.Unlock()
	return encode("oauth2accesstoken", tok, host), nil
}

// fetchGCEMetadataToken asks the GCE metadata server for the default
// service account's access token (the same token `docker login` with
// the metadata SA would use).
func fetchGCEMetadataToken(ctx context.Context) (string, time.Time, error) {
	raw, err := metadata.GetWithContext(ctx, "instance/service-accounts/default/token")
	if err != nil {
		return "", time.Time{}, err
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal([]byte(raw), &tok); err != nil {
		return "", time.Time{}, err
	}
	return tok.AccessToken, time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second), nil
}
