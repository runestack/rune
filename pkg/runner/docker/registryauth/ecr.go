package registryauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
)

type ECRConfig struct {
	Registry string // pattern, e.g., *.dkr.ecr.us-east-1.amazonaws.com
	Region   string // optional override
}

type ECRProvider struct {
	cfg   ECRConfig
	mu    sync.Mutex
	cache map[string]ecrEntry // host -> entry
}

type ecrEntry struct {
	Username string
	Password string
	Expires  time.Time
}

func NewECRProvider(cfg ECRConfig) *ECRProvider {
	return &ECRProvider{cfg: cfg, cache: make(map[string]ecrEntry)}
}

func (p *ECRProvider) Match(host, imageRef string) bool {
	return patternMatches(p.cfg.Registry, host, imageRef)
}

// Pattern implements Scoped.
func (p *ECRProvider) Pattern() string { return p.cfg.Registry }

func (p *ECRProvider) Resolve(ctx context.Context, host, imageRef string) (string, error) {
	p.mu.Lock()
	if ent, ok := p.cache[host]; ok {
		if time.Until(ent.Expires) > 5*time.Minute {
			p.mu.Unlock()
			return encode(ent.Username, ent.Password, host), nil
		}
	}
	p.mu.Unlock()

	username, password, exp, err := p.fetch(ctx, host)
	if err != nil {
		return "", nil // fallback to anonymous
	}
	p.mu.Lock()
	p.cache[host] = ecrEntry{Username: username, Password: password, Expires: exp}
	p.mu.Unlock()
	return encode(username, password, host), nil
}

func (p *ECRProvider) fetch(ctx context.Context, host string) (string, string, time.Time, error) {
	region := p.cfg.Region
	if region == "" {
		parts := strings.Split(host, ".")
		if len(parts) >= 6 {
			region = parts[3]
		}
	}
	if region == "" {
		return "", "", time.Time{}, fmt.Errorf("ecr: no region for host %s", host)
	}
	cfg, err := awscfg.LoadDefaultConfig(ctx, awscfg.WithRegion(region))
	if err != nil {
		return "", "", time.Time{}, err
	}
	cli := ecr.NewFromConfig(cfg)
	out, err := cli.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", "", time.Time{}, err
	}
	if len(out.AuthorizationData) == 0 {
		return "", "", time.Time{}, fmt.Errorf("ecr: empty auth data")
	}
	var chosen ecrtypes.AuthorizationData
	for _, ad := range out.AuthorizationData {
		if ad.ProxyEndpoint != nil && strings.Contains(*ad.ProxyEndpoint, host) {
			chosen = ad
			break
		}
	}
	if chosen.AuthorizationToken == nil {
		chosen = out.AuthorizationData[0]
	}
	tok, err := base64.StdEncoding.DecodeString(*chosen.AuthorizationToken)
	if err != nil {
		return "", "", time.Time{}, err
	}
	parts := strings.SplitN(string(tok), ":", 2)
	if len(parts) != 2 {
		return "", "", time.Time{}, fmt.Errorf("ecr: invalid token format")
	}
	exp := time.Now().Add(12 * time.Hour)
	if chosen.ExpiresAt != nil {
		exp = *chosen.ExpiresAt
	}
	return parts[0], parts[1], exp, nil
}

func encode(username, password, host string) string {
	payload := map[string]string{
		"username":      username,
		"password":      password,
		"serveraddress": host,
	}
	b, _ := json.Marshal(payload)
	return base64.StdEncoding.EncodeToString(b)
}
