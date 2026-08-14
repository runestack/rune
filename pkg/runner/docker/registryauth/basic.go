package registryauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
)

type BasicTokenConfig struct {
	Registry string
	Username string
	Password string
	Token    string

	// AnonymousOnly marks this pattern as deliberately credential-free.
	// Resolve returns no auth, so a narrow anonymous entry can override a
	// broader credentialed one for a public repo on a private registry.
	AnonymousOnly bool
}

type BasicTokenProvider struct {
	cfg BasicTokenConfig
}

func NewBasicTokenProvider(cfg BasicTokenConfig) *BasicTokenProvider {
	return &BasicTokenProvider{cfg: cfg}
}

func (p *BasicTokenProvider) Match(host, imageRef string) bool {
	return patternMatches(p.cfg.Registry, host, imageRef)
}

// Pattern implements Scoped so the resolver can rank this provider.
func (p *BasicTokenProvider) Pattern() string { return p.cfg.Registry }

// IsAnonymous implements Anonymous.
func (p *BasicTokenProvider) IsAnonymous() bool { return p.cfg.AnonymousOnly }

func (p *BasicTokenProvider) Resolve(ctx context.Context, host, imageRef string) (string, error) {
	if p.cfg.AnonymousOnly {
		return "", nil
	}
	username := p.cfg.Username
	password := p.cfg.Password
	if username == "" && password == "" && p.cfg.Token != "" {
		username = "token"
		password = p.cfg.Token
	}
	if username == "" || password == "" {
		return "", nil
	}
	payload := map[string]string{
		"username":      username,
		"password":      password,
		"serveraddress": host,
	}
	b, _ := json.Marshal(payload)
	return base64.StdEncoding.EncodeToString(b), nil
}
