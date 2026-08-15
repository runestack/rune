package registryauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
)

// DockerConfigJSONProvider resolves credentials from a .dockerconfigjson blob
type DockerConfigJSONProvider struct {
	registryPattern string
	rawJSON         string
}

func NewDockerConfigJSONProvider(registryPattern, raw string) *DockerConfigJSONProvider {
	return &DockerConfigJSONProvider{registryPattern: registryPattern, rawJSON: raw}
}

func (p *DockerConfigJSONProvider) Match(host, imageRef string) bool {
	return patternMatches(p.registryPattern, host, imageRef)
}

// Pattern implements Scoped.
func (p *DockerConfigJSONProvider) Pattern() string { return p.registryPattern }

func (p *DockerConfigJSONProvider) Resolve(ctx context.Context, host, imageRef string) (string, error) {
	// Minimal parsing: look under auths for an entry matching host
	// Spec: { "auths": { "<server>": {"auth":"base64(user:pass)", "identitytoken":"..." } } }
	var dcj struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(p.rawJSON), &dcj); err != nil {
		return "", nil
	}
	// Try exact host and known Docker Hub key
	candidates := []string{host, "https://index.docker.io/v1/"}
	for key, v := range dcj.Auths {
		for _, cand := range candidates {
			if key == cand || strings.Contains(key, host) {
				if v.Auth != "" {
					// decode user:pass
					dec, err := base64.StdEncoding.DecodeString(v.Auth)
					if err != nil {
						continue
					}
					parts := strings.SplitN(string(dec), ":", 2)
					if len(parts) != 2 {
						continue
					}
					return encode(parts[0], parts[1], host), nil
				}
				if v.IdentityToken != "" {
					// use identity token as password with username "token"
					return encode("token", v.IdentityToken, host), nil
				}
			}
		}
	}
	return "", nil
}
