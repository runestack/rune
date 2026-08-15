package registryauth

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeDockerConfig(t *testing.T, content string) *DockerConfigFileProvider {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return &DockerConfigFileProvider{path: path}
}

func TestDockerConfigFileProviderInlineAuths(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("user:pass"))
	p := writeDockerConfig(t, `{"auths": {"europe-west2-docker.pkg.dev": {"auth": "`+auth+`"}}}`)

	if !p.Match("europe-west2-docker.pkg.dev", "europe-west2-docker.pkg.dev/proj/app:v1") {
		t.Fatal("provider should match any host when config file exists")
	}
	b64, err := p.Resolve(context.Background(), "europe-west2-docker.pkg.dev", "europe-west2-docker.pkg.dev/p/r/app:1")
	if err != nil {
		t.Fatal(err)
	}
	m, err := decode(b64)
	if err != nil {
		t.Fatal(err)
	}
	if m["username"] != "user" || m["password"] != "pass" {
		t.Fatalf("unexpected creds: %+v", m)
	}
}

func TestDockerConfigFileProviderHTTPSPrefixedKey(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("u:p"))
	p := writeDockerConfig(t, `{"auths": {"https://ghcr.io": {"auth": "`+auth+`"}}}`)
	b64, err := p.Resolve(context.Background(), "ghcr.io", "ghcr.io/acme/app:1")
	if err != nil {
		t.Fatal(err)
	}
	if b64 == "" {
		t.Fatal("expected auth resolved from https://-prefixed key")
	}
}

func TestDockerConfigFileProviderNoEntryIsAnonymous(t *testing.T) {
	p := writeDockerConfig(t, `{"auths": {}}`)
	b64, err := p.Resolve(context.Background(), "ghcr.io", "ghcr.io/acme/app:1")
	if err != nil {
		t.Fatal(err)
	}
	if b64 != "" {
		t.Fatalf("expected anonymous for unknown host, got %q", b64)
	}
}

func TestDockerConfigFileProviderMissingFile(t *testing.T) {
	p := &DockerConfigFileProvider{path: filepath.Join(t.TempDir(), "nope", "config.json")}
	if p.Match("ghcr.io", "ghcr.io/org/app:v1") {
		t.Fatal("provider should not match when config file is absent")
	}
	b64, err := p.Resolve(context.Background(), "ghcr.io", "ghcr.io/acme/app:1")
	if err != nil || b64 != "" {
		t.Fatalf("expected anonymous no-op, got %q / %v", b64, err)
	}
}

func TestDockerConfigFileProviderCredHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake helper")
	}
	// Fake docker-credential-test on PATH
	binDir := t.TempDir()
	helper := filepath.Join(binDir, "docker-credential-test")
	script := `#!/bin/sh
read host
echo "{\"ServerURL\":\"$host\",\"Username\":\"oauth2accesstoken\",\"Secret\":\"ya29.tok\"}"
`
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := writeDockerConfig(t, `{"credHelpers": {"europe-west2-docker.pkg.dev": "test"}}`)
	b64, err := p.Resolve(context.Background(), "europe-west2-docker.pkg.dev", "europe-west2-docker.pkg.dev/p/r/app:1")
	if err != nil {
		t.Fatal(err)
	}
	m, err := decode(b64)
	if err != nil {
		t.Fatal(err)
	}
	if m["username"] != "oauth2accesstoken" || m["password"] != "ya29.tok" {
		t.Fatalf("unexpected creds from helper: %+v", m)
	}
}

func TestDockerConfigFileProviderCredHelperFailureFallsThrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake helper")
	}
	binDir := t.TempDir()
	helper := filepath.Join(binDir, "docker-credential-broken")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Helper fails but an inline auths entry exists → falls through to it.
	auth := base64.StdEncoding.EncodeToString([]byte("user:pass"))
	p := writeDockerConfig(t, `{"credHelpers": {"ghcr.io": "broken"}, "auths": {"ghcr.io": {"auth": "`+auth+`"}}}`)
	b64, err := p.Resolve(context.Background(), "ghcr.io", "ghcr.io/acme/app:1")
	if err != nil {
		t.Fatal(err)
	}
	m, err := decode(b64)
	if err != nil {
		t.Fatal(err)
	}
	if m["username"] != "user" {
		t.Fatalf("expected fall-through to inline auths, got %+v", m)
	}
}

func TestDockerConfigFileProviderIdentityTokenHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake helper")
	}
	binDir := t.TempDir()
	helper := filepath.Join(binDir, "docker-credential-idtok")
	script := `#!/bin/sh
read host
echo "{\"ServerURL\":\"$host\",\"Username\":\"<token>\",\"Secret\":\"idtok\"}"
`
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := writeDockerConfig(t, `{"credsStore": "idtok"}`)
	b64, err := p.Resolve(context.Background(), "registry.example.com", "registry.example.com/app:1")
	if err != nil {
		t.Fatal(err)
	}
	m, err := decode(b64)
	if err != nil {
		t.Fatal(err)
	}
	// mirrors DockerConfigJSONProvider's identity-token convention
	if m["username"] != "token" || m["password"] != "idtok" {
		t.Fatalf("unexpected creds: %+v", m)
	}
}
