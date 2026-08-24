package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMintNodeID(t *testing.T) {
	tests := []struct {
		name      string
		preferred string
		hostname  string
		want      string
		wantHex   bool
	}{
		{name: "explicit name wins", preferred: "gpu-1", hostname: "box", want: "gpu-1"},
		{name: "hostname when no explicit name", hostname: "gpu-node-2", want: "gpu-node-2"},
		{name: "hostname is lowercased", hostname: "GPU-Node-3", want: "gpu-node-3"},
		{name: "invalid explicit name falls through to hostname", preferred: "not_a_label", hostname: "box", want: "box"},
		{name: "no hostname falls back to hex", wantHex: true},
		{name: "invalid hostname falls back to hex", hostname: "box.example.com", wantHex: true},
		{name: "over-long hostname falls back to hex", hostname: strings.Repeat("a", 64), wantHex: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mintNodeID(tt.preferred, tt.hostname)
			if tt.wantHex {
				if !strings.HasPrefix(got, "node-") || len(got) != len("node-")+16 {
					t.Fatalf("mintNodeID(%q, %q) = %q, want node-<16 hex>", tt.preferred, tt.hostname, got)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("mintNodeID(%q, %q) = %q, want %q", tt.preferred, tt.hostname, got, tt.want)
			}
		})
	}
}

// An existing node-identity.json is never rewritten, whatever --node-name
// says. Volume.BoundNode rows on disk already carry the minted ID, so
// renaming in place would unmount every volume.
func TestLoadOrCreateIdentity_ExistingIsNeverRenamed(t *testing.T) {
	dir := t.TempDir()
	seed := Identity{NodeID: "node-deadbeefdeadbeef", Hostname: "old-host"}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node-identity.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadOrCreateIdentity(dir, "brand-new-name")
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	if got.NodeID != seed.NodeID {
		t.Fatalf("NodeID = %q, want the persisted %q — an existing identity must never be renamed", got.NodeID, seed.NodeID)
	}
}

func TestLoadOrCreateIdentity_MintsPreferredNameOnFirstBoot(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadOrCreateIdentity(dir, "gpu-1")
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity: %v", err)
	}
	if got.NodeID != "gpu-1" {
		t.Fatalf("NodeID = %q, want %q", got.NodeID, "gpu-1")
	}
	// And it is persisted, so the second boot agrees.
	again, err := LoadOrCreateIdentity(dir, "something-else")
	if err != nil {
		t.Fatalf("LoadOrCreateIdentity (second): %v", err)
	}
	if again.NodeID != "gpu-1" {
		t.Fatalf("second load NodeID = %q, want %q", again.NodeID, "gpu-1")
	}
}

// The hex fallback must be reachable on the path that actually exists.
// os.Hostname() failing substitutes a placeholder for the Hostname field,
// and that placeholder is a valid DNS-1123 label — so if it reached the
// minter it would become the node's ID and name every such machine the
// same thing.
func TestHostnameForIdentity(t *testing.T) {
	record, mint := hostnameForIdentity("gpu-1", nil)
	if record != "gpu-1" || mint != "gpu-1" {
		t.Fatalf("hostnameForIdentity(\"gpu-1\", nil) = (%q, %q), want both %q", record, mint, "gpu-1")
	}

	record, mint = hostnameForIdentity("", errors.New("no hostname"))
	if record != "unknown" {
		t.Fatalf("unreadable hostname recorded as %q, want %q", record, "unknown")
	}
	if mint != "" {
		t.Fatalf("unreadable hostname minted from %q, want \"\" so the ID falls back to node-<hex>", mint)
	}
	// And that empty value is what makes the hex branch reachable.
	if got := mintNodeID("", mint); !strings.HasPrefix(got, "node-") {
		t.Fatalf("mintNodeID with no hostname = %q, want a node-<hex> name", got)
	}
}

// The minter itself does NOT special-case the placeholder string: a
// machine genuinely named "unknown" is a real machine and keeps its name.
// The discrimination belongs at the call site, where the error is known.
func TestMintNodeID_DoesNotSpecialCaseThePlaceholder(t *testing.T) {
	if got := mintNodeID("", "unknown"); got != "unknown" {
		t.Fatalf("mintNodeID(\"\", \"unknown\") = %q, want %q", got, "unknown")
	}
}
