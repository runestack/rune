package docker

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/log"
)

// newAuthTestRunner builds a runner with only the registry config populated.
// resolveRegistryAuth needs no Docker daemon, so these stay pure unit tests.
func newAuthTestRunner(t *testing.T, regs ...RegistryConfig) *DockerRunner {
	t.Helper()
	cfg := DefaultDockerConfig()
	cfg.Registries = regs
	// Ambient providers read the host's docker config / GCE metadata; disable
	// them so these assertions are about the configured entries only.
	cfg.DisableAmbientRegistryAuth = true
	return &DockerRunner{
		logger: log.NewLogger(),
		config: cfg,
	}
}

func decodedUser(t *testing.T, auth string) string {
	t.Helper()
	if auth == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		t.Fatalf("decode auth: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal auth: %v", err)
	}
	return payload["username"]
}

// TestResolveRegistryAuth_HostWideStillApplies: the historical behaviour must
// be preserved — a host-only entry covers every image on that host.
func TestResolveRegistryAuth_HostWideStillApplies(t *testing.T) {
	r := newAuthTestRunner(t, RegistryConfig{
		Registry: "ghcr.io",
		Auth:     RegistryAuth{Type: "basic", Username: "hostwide", Password: "pw"},
	})
	if got := decodedUser(t, r.resolveRegistryAuth("ghcr.io/anyorg/app:v1")); got != "hostwide" {
		t.Fatalf("host-wide credential should apply, got user %q", got)
	}
}

// TestResolveRegistryAuth_ScopedDoesNotLeakToOtherRepos is the #178 fix in
// end-to-end form: this is the incident. A credential scoped to the org's own
// repositories must NOT be attached to an unrelated public image on the same
// registry — that attachment is what turned an expired token into a hard
// failure for ghcr.io/floruntime/flo, which needs no credential at all.
func TestResolveRegistryAuth_ScopedDoesNotLeakToOtherRepos(t *testing.T) {
	r := newAuthTestRunner(t, RegistryConfig{
		Registry: "ghcr.io/myorg",
		Auth:     RegistryAuth{Type: "basic", Username: "myorg-bot", Password: "pw"},
	})

	if got := decodedUser(t, r.resolveRegistryAuth("ghcr.io/myorg/private-app:v1")); got != "myorg-bot" {
		t.Fatalf("scoped credential should apply to its own repo, got user %q", got)
	}
	if auth := r.resolveRegistryAuth("ghcr.io/floruntime/flo:0.1.0-dev.9"); auth != "" {
		t.Fatalf("scoped credential must NOT be attached to another org's public image, got %q", auth)
	}
}

// TestResolveRegistryAuth_MostSpecificWins: a narrow entry beats a broad one
// regardless of declaration order.
func TestResolveRegistryAuth_MostSpecificWins(t *testing.T) {
	broadFirst := newAuthTestRunner(t,
		RegistryConfig{Registry: "ghcr.io", Auth: RegistryAuth{Type: "basic", Username: "broad", Password: "pw"}},
		RegistryConfig{Registry: "ghcr.io/myorg", Auth: RegistryAuth{Type: "basic", Username: "narrow", Password: "pw"}},
	)
	if got := decodedUser(t, broadFirst.resolveRegistryAuth("ghcr.io/myorg/app:v1")); got != "narrow" {
		t.Fatalf("most specific entry should win, got user %q", got)
	}
	// Reversed declaration order must not change the outcome.
	narrowFirst := newAuthTestRunner(t,
		RegistryConfig{Registry: "ghcr.io/myorg", Auth: RegistryAuth{Type: "basic", Username: "narrow", Password: "pw"}},
		RegistryConfig{Registry: "ghcr.io", Auth: RegistryAuth{Type: "basic", Username: "broad", Password: "pw"}},
	)
	if got := decodedUser(t, narrowFirst.resolveRegistryAuth("ghcr.io/myorg/app:v1")); got != "narrow" {
		t.Fatalf("precedence must not depend on declaration order, got user %q", got)
	}
	// An image outside the narrow scope still gets the broad credential.
	if got := decodedUser(t, broadFirst.resolveRegistryAuth("ghcr.io/other/app:v1")); got != "broad" {
		t.Fatalf("host-wide entry should still cover unscoped repos, got user %q", got)
	}
}

// TestResolveRegistryAuth_AnonymousEntryOverridesBroadCredential: an operator
// declares one repository public on an otherwise-credentialed registry, and no
// token is sent for it.
func TestResolveRegistryAuth_AnonymousEntryOverridesBroadCredential(t *testing.T) {
	r := newAuthTestRunner(t,
		RegistryConfig{Registry: "ghcr.io", Auth: RegistryAuth{Type: "basic", Username: "broad", Password: "pw"}},
		RegistryConfig{Registry: "ghcr.io/floruntime", Auth: RegistryAuth{Type: "none"}},
	)
	if auth := r.resolveRegistryAuth("ghcr.io/floruntime/flo:0.1.0-dev.9"); auth != "" {
		t.Fatalf("anonymous entry must suppress the broader credential, got %q", auth)
	}
	// Everything else on the host keeps the credential.
	if got := decodedUser(t, r.resolveRegistryAuth("ghcr.io/myorg/app:v1")); got != "broad" {
		t.Fatalf("anonymous entry must not disable auth for other repos, got user %q", got)
	}
}

// TestAnnotatePullError_NamesTheCredential covers the #177 diagnostics: a bare
// "denied" gave no hint that a CONFIGURED credential was involved, which is
// what made the incident read as "this image must be private".
func TestAnnotatePullError_NamesTheCredential(t *testing.T) {
	r := newAuthTestRunner(t, RegistryConfig{
		Registry: "ghcr.io",
		Auth:     RegistryAuth{Type: "basic", Username: "u", Password: "pw"},
	})
	const image = "ghcr.io/floruntime/flo:0.1.0-dev.9"

	// Resolve first so the runner records which pattern supplied the auth.
	if auth := r.resolveRegistryAuth(image); auth == "" {
		t.Fatal("expected the host-wide credential to be used for this image")
	}

	err := r.annotatePullError(image, true, errors.New("error from registry: denied"))
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "denied") {
		t.Errorf("original registry error must be preserved: %q", msg)
	}
	if !strings.Contains(msg, "ghcr.io") {
		t.Errorf("error should name the registry pattern that supplied the credential: %q", msg)
	}
	if !strings.Contains(msg, "rejected") {
		t.Errorf("error should say the credential was rejected: %q", msg)
	}
}

// When no credential was used, the message must point the other way — the
// registry wants credentials and none are configured.
func TestAnnotatePullError_NoCredentialConfigured(t *testing.T) {
	r := newAuthTestRunner(t)
	err := r.annotatePullError("ghcr.io/private/app:v1", false, errors.New("unauthorized"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no [[docker.registries]] entry matches") {
		t.Errorf("should say no credential is configured: %q", err.Error())
	}
}

// Non-auth failures must pass through untouched — we must not relabel a
// network blip or a missing tag as a credential problem.
func TestAnnotatePullError_LeavesNonAuthErrorsAlone(t *testing.T) {
	r := newAuthTestRunner(t)
	orig := errors.New("manifest unknown")
	if got := r.annotatePullError("ghcr.io/o/a:v1", true, orig); got != orig {
		t.Fatalf("non-auth error should be returned unchanged, got %q", got)
	}
	if got := r.annotatePullError("ghcr.io/o/a:v1", true, nil); got != nil {
		t.Fatalf("nil error must stay nil, got %v", got)
	}
}
