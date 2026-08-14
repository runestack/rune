package registryauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DockerConfigFileProvider resolves credentials from the ambient
// docker CLI config of the user runed runs as ($DOCKER_CONFIG/config.json
// or $HOME/.docker/config.json), honoring inline auths as well as
// credHelpers/credsStore credential helpers. This is what makes a
// plain `docker login` on the node work for runed pulls — the Docker
// Go SDK does not read this file itself; populating RegistryAuth is
// the caller's job (issue #144).
//
// The file is re-read on every Resolve so a fresh `docker login`
// takes effect without a runed restart. Pulls are rare enough that
// the extra stat/read is noise.
type DockerConfigFileProvider struct {
	// path overrides config discovery in tests; empty means resolve
	// from DOCKER_CONFIG / $HOME at Resolve time.
	path string
}

func NewDockerConfigFileProvider() *DockerConfigFileProvider {
	return &DockerConfigFileProvider{}
}

// Match is intentionally broad: this provider is appended after all
// configured providers as an ambient fallback, and Resolve returns ""
// (anonymous) when the config has no entry for the host.
func (p *DockerConfigFileProvider) Match(host, imageRef string) bool {
	_, err := os.Stat(p.configPath())
	return err == nil
}

func (p *DockerConfigFileProvider) configPath() string {
	if p.path != "" {
		return p.path
	}
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".docker", "config.json")
}

// dockerCLIConfig is the subset of ~/.docker/config.json we consume.
type dockerCLIConfig struct {
	Auths map[string]struct {
		Auth          string `json:"auth"`
		IdentityToken string `json:"identitytoken"`
	} `json:"auths"`
	CredsStore  string            `json:"credsStore"`
	CredHelpers map[string]string `json:"credHelpers"`
}

func (p *DockerConfigFileProvider) Resolve(ctx context.Context, host, imageRef string) (string, error) {
	raw, err := os.ReadFile(p.configPath())
	if err != nil {
		return "", nil
	}
	var cfg dockerCLIConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", nil
	}

	// Resolution order matches the docker CLI: per-host credHelper,
	// then the global credsStore, then inline auths. A failing helper
	// falls through to the next source rather than erroring — the
	// provider chain treats "" as "try the next provider / anonymous".
	if helper := cfg.CredHelpers[host]; helper != "" {
		if auth := execCredentialHelper(ctx, helper, host); auth != "" {
			return auth, nil
		}
	}
	if cfg.CredsStore != "" {
		if auth := execCredentialHelper(ctx, cfg.CredsStore, host); auth != "" {
			return auth, nil
		}
	}
	return resolveInlineAuths(cfg, host), nil
}

// resolveInlineAuths handles the auths{} map, using the same key
// candidates as DockerConfigJSONProvider (exact host, https:// prefix,
// legacy Docker Hub v1 key).
func resolveInlineAuths(cfg dockerCLIConfig, host string) string {
	candidates := []string{host, "https://" + host, "https://index.docker.io/v1/"}
	for key, v := range cfg.Auths {
		for _, cand := range candidates {
			if key != cand && !strings.Contains(key, host) {
				continue
			}
			if v.Auth != "" {
				dec, err := base64.StdEncoding.DecodeString(v.Auth)
				if err != nil {
					continue
				}
				parts := strings.SplitN(string(dec), ":", 2)
				if len(parts) != 2 {
					continue
				}
				return encode(parts[0], parts[1], host)
			}
			if v.IdentityToken != "" {
				// same convention as DockerConfigJSONProvider
				return encode("token", v.IdentityToken, host)
			}
		}
	}
	return ""
}

// execCredentialHelper runs `docker-credential-<name> get` with the
// registry host on stdin, per the docker-credential-helpers protocol.
// Any failure (helper missing, host not stored, malformed output)
// yields "" so the chain can fall through.
func execCredentialHelper(ctx context.Context, name, host string) string {
	cmd := exec.CommandContext(ctx, "docker-credential-"+name, "get")
	cmd.Stdin = strings.NewReader(host)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	var creds struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}
	if err := json.Unmarshal(out.Bytes(), &creds); err != nil {
		return ""
	}
	if creds.Secret == "" {
		return ""
	}
	if creds.Username == "" || creds.Username == "<token>" {
		// identity-token convention, mirroring DockerConfigJSONProvider
		return encode("token", creds.Secret, host)
	}
	return encode(creds.Username, creds.Secret, host)
}
