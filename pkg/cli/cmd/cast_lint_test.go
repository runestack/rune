package cmd

import (
	"strings"
	"testing"

	"github.com/runestack/rune/pkg/types"
)

// An invalid DNS-1123 resource name must be caught by the rendered-cast lint
// (before plan/apply), not deferred to a mid-apply server error.
func TestRenderedRelease_LintCatchesBadName(t *testing.T) {
	r := &renderedRelease{payloads: newCastPayloads()}
	r.payloads.configmaps["configmap/default/Bad_Name!"] = &types.Configmap{
		Name: "Bad_Name!", Namespace: "default", Data: map[string]string{"k": "v"},
	}
	err := r.lint()
	if err == nil {
		t.Fatal("expected lint error for invalid configmap name, got nil")
	}
	if !strings.Contains(err.Error(), "Bad_Name!") || !strings.Contains(err.Error(), "invalid name") {
		t.Errorf("error should name the offending resource: %v", err)
	}
}

// An invalid namespace is also caught.
func TestRenderedRelease_LintCatchesBadNamespace(t *testing.T) {
	r := &renderedRelease{payloads: newCastPayloads()}
	r.payloads.secrets["secret/Bad_NS/creds"] = &types.Secret{
		Name: "creds", Namespace: "Bad_NS", Data: map[string]string{"t": "x"},
	}
	if err := r.lint(); err == nil {
		t.Fatal("expected lint error for invalid namespace, got nil")
	}
}

// Valid names pass cleanly.
func TestRenderedRelease_LintPassesValid(t *testing.T) {
	r := &renderedRelease{payloads: newCastPayloads()}
	r.payloads.configmaps["configmap/default/app-settings"] = &types.Configmap{
		Name: "app-settings", Namespace: "default", Data: map[string]string{"k": "v"},
	}
	r.payloads.secrets["secret/default/creds"] = &types.Secret{
		Name: "creds", Namespace: "default", Data: map[string]string{"t": "x"},
	}
	if err := r.lint(); err != nil {
		t.Fatalf("expected no lint error for valid resources, got %v", err)
	}
}

// The advisory findings must reach a plain `rune cast`, not just `rune lint`:
// the operator who never runs lint is the one the warnings are for (#208).
func TestRenderedRelease_UpdateWarningsSurfaceOnCast(t *testing.T) {
	r := &renderedRelease{payloads: newCastPayloads()}
	r.payloads.services["service/default/db"] = &types.Service{
		Name: "db", Namespace: "default", Image: "postgres:16", Command: "postgres",
		Runtime: types.RuntimeTypeContainer, Scale: 1,
		UpdateStrategy: &types.UpdateStrategy{Type: types.UpdateRolling},
		Volumes: []types.VolumeMount{{Name: "data", MountPath: "/data", ClaimTemplate: &types.VolumeClaimTemplate{
			StorageClassName: "local-host", Size: "1Gi", AccessMode: types.AccessModeRWO}}},
	}
	r.payloads.services["service/default/api"] = &types.Service{
		Name: "api", Namespace: "default", Image: "api:1", Command: "api",
		Runtime: types.RuntimeTypeContainer, Scale: 3,
		Ports: []types.ServicePort{{Name: "http", Port: 8080}},
	}

	warns := r.updateWarnings()
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, "update-recreates-stateful") {
		t.Errorf("stateful service must be warned about on cast: %v", warns)
	}
	if !strings.Contains(joined, "db: ") || !strings.Contains(joined, "api: ") {
		t.Errorf("every finding must name its service: %v", warns)
	}
	// Map iteration is random; the printed order must not be.
	for i := 0; i < 8; i++ {
		if got := strings.Join(r.updateWarnings(), "\n"); got != joined {
			t.Fatalf("warning order is not stable:\n%s\n---\n%s", joined, got)
		}
	}

	var buf strings.Builder
	printUpdateWarnings(&buf, warns)
	if !strings.Contains(buf.String(), "⚠") {
		t.Errorf("warnings must print with the same glyph rune lint uses: %q", buf.String())
	}

	// A service with nothing to say prints nothing at all — no blank block.
	quiet := &renderedRelease{payloads: newCastPayloads()}
	quiet.payloads.services["service/default/worker"] = &types.Service{
		Name: "worker", Namespace: "default", Image: "w:1", Command: "w",
		Runtime: types.RuntimeTypeContainer, Scale: 3,
	}
	buf.Reset()
	printUpdateWarnings(&buf, quiet.updateWarnings())
	if buf.String() != "" {
		t.Errorf("a clean release must print no warning block, got %q", buf.String())
	}
}
