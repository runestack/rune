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
