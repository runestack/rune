package docker

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func decodeAuth(b64 string) (map[string]string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func TestParseImageHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ghcr.io/acme/app:1.0", "ghcr.io"},
		{"123456789012.dkr.ecr.us-east-1.amazonaws.com/repo:tag", "123456789012.dkr.ecr.us-east-1.amazonaws.com"},
		{"nginx:alpine", "index.docker.io"},
		{"localhost:5000/repo", "localhost:5000"},
	}
	for _, c := range cases {
		got := parseImageHost(c.in)
		if got != c.want {
			t.Fatalf("parseImageHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveRegistryAuth_BasicExactAndWildcard(t *testing.T) {
	r := &registryAuthResolver{
		logger: nil,
		config: &DockerConfig{
			// Keep the test hermetic: without this the ambient
			// fallback would read the developer's real
			// ~/.docker/config.json and break the "expected empty"
			// assertion below on any logged-in machine.
			DisableAmbientRegistryAuth: true,
			Registries: []RegistryConfig{
				{Registry: "ghcr.io", Auth: RegistryAuth{Type: "basic", Username: "u", Password: "p"}},
				{Registry: "*.internal.registry.local", Auth: RegistryAuth{Type: "basic", Username: "wu", Password: "wp"}},
			}},
	}

	// exact host
	auth := r.resolveRegistryAuth("ghcr.io/acme/app:1.0")
	if auth == "" {
		t.Fatal("expected non-empty auth for ghcr.io")
	}
	m, err := decodeAuth(auth)
	if err != nil {
		t.Fatal(err)
	}
	if m["username"] != "u" || m["password"] != "p" || !strings.Contains(m["serveraddress"], "ghcr.io") {
		t.Fatalf("unexpected auth payload: %+v", m)
	}

	// wildcard host
	auth2 := r.resolveRegistryAuth("a.internal.registry.local/team/app:2")
	if auth2 == "" {
		t.Fatal("expected non-empty auth for wildcard")
	}
	m2, err := decodeAuth(auth2)
	if err != nil {
		t.Fatal(err)
	}
	if m2["username"] != "wu" || m2["password"] != "wp" {
		t.Fatalf("unexpected wildcard auth payload: %+v", m2)
	}

	// docker hub (not configured) should be empty
	if got := r.resolveRegistryAuth("nginx:alpine"); got != "" {
		t.Fatalf("expected empty auth for docker hub, got %q", got)
	}
}

// The ambient docker-config fallback: with no [[docker.registries]]
// entry matching, a `docker login` of the runed user must still
// authenticate pulls (issue #144, gap 2).
func TestResolveRegistryAuth_AmbientDockerConfigFallback(t *testing.T) {
	dir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("au:ap"))
	cfgJSON := `{"auths": {"registry.example.com": {"auth": "` + auth + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)

	r := &registryAuthResolver{config: &DockerConfig{}}
	got := r.resolveRegistryAuth("registry.example.com/team/app:1")
	if got == "" {
		t.Fatal("expected ambient docker-config fallback to resolve auth")
	}
	m, err := decodeAuth(got)
	if err != nil {
		t.Fatal(err)
	}
	if m["username"] != "au" || m["password"] != "ap" {
		t.Fatalf("unexpected ambient auth payload: %+v", m)
	}
}

// Explicit [[docker.registries]] config must win over ambient sources.
func TestResolveRegistryAuth_ConfiguredWinsOverAmbient(t *testing.T) {
	dir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("ambient:creds"))
	cfgJSON := `{"auths": {"registry.example.com": {"auth": "` + auth + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)

	r := &registryAuthResolver{config: &DockerConfig{Registries: []RegistryConfig{
		{Registry: "registry.example.com", Auth: RegistryAuth{Type: "basic", Username: "cfg", Password: "cfgpw"}},
	}}}
	m, err := decodeAuth(r.resolveRegistryAuth("registry.example.com/team/app:1"))
	if err != nil {
		t.Fatal(err)
	}
	if m["username"] != "cfg" {
		t.Fatalf("expected configured provider to win, got %+v", m)
	}
}
