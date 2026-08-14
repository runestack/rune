package registryauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func decode(b64 string) (map[string]string, error) {
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

func TestDockerConfigJSONProvider(t *testing.T) {
	// Build dockerconfigjson with auth
	auth := base64.StdEncoding.EncodeToString([]byte("user:pass"))
	dcj := `{"auths": {"ghcr.io": {"auth": "` + auth + `"}}}`

	p := NewDockerConfigJSONProvider("ghcr.io", dcj)
	if !p.Match("ghcr.io", "ghcr.io/org/app:v1") {
		t.Fatal("provider should match ghcr.io")
	}
	b64, err := p.Resolve(context.Background(), "ghcr.io", "ghcr.io/acme/app:1.0")
	if err != nil {
		t.Fatal(err)
	}
	if b64 == "" {
		t.Fatal("expected non-empty auth")
	}
	m, err := decode(b64)
	if err != nil {
		t.Fatal(err)
	}
	if m["username"] != "user" || m["password"] != "pass" {
		t.Fatalf("unexpected creds: %+v", m)
	}
}

func TestFactoryBuildProviders(t *testing.T) {
	regs := []map[string]any{
		{"registry": "ghcr.io", "auth": map[string]any{"type": "basic", "username": "u", "password": "p"}},
		{"registry": "*.dkr.ecr.us-east-1.amazonaws.com", "auth": map[string]any{"type": "ecr", "region": "us-east-1"}},
		{"registry": "index.docker.io", "auth": map[string]any{"type": "dockerconfigjson", "dockerconfigjson": `{"auths":{"https://index.docker.io/v1/":{"identitytoken":"idtok"}}}`}},
	}
	ps := BuildProviders(context.Background(), regs)
	if len(ps) != 3 {
		t.Fatalf("expected 3 providers, got %d", len(ps))
	}
}
