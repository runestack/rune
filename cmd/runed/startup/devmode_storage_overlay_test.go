package startup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/log"
)

func TestApplyDevModeStorageOverlay_Defaults(t *testing.T) {
	s := &config.Storage{}
	applyDevModeStorageOverlay(s, log.GetDefaultLogger())

	if s.Drivers == nil {
		t.Fatalf("Drivers map should be initialised")
	}
	host := s.Drivers["local-host"]
	if host == nil {
		t.Fatalf("local-host driver overlay missing")
	}
	if v, _ := host["allowCreateMissing"].(bool); !v {
		t.Errorf("expected allowCreateMissing=true, got %v", host["allowCreateMissing"])
	}
	allow, _ := host["hostPathAllowlist"].([]any)
	if len(allow) == 0 {
		t.Fatalf("expected at least one entry in hostPathAllowlist, got %v", host["hostPathAllowlist"])
	}
	want := mustHomeRel(t)
	found := false
	for _, v := range allow {
		if s, ok := v.(string); ok && s == want {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %q in hostPathAllowlist, got %v", want, allow)
	}

	mgr := s.Drivers["local"]
	if mgr == nil {
		t.Fatalf("local driver overlay missing")
	}
	if got, _ := mgr["localVolumeRoot"].(string); got != want {
		t.Errorf("expected localVolumeRoot=%q, got %v", want, mgr["localVolumeRoot"])
	}
}

func TestApplyDevModeStorageOverlay_RespectsOperatorValues(t *testing.T) {
	s := &config.Storage{
		Drivers: map[string]map[string]any{
			"local-host": {
				"allowCreateMissing": false,
				"hostPathAllowlist":  []any{"/srv/rune"},
			},
			"local": {
				"localVolumeRoot": "/var/custom/volumes",
			},
		},
	}
	applyDevModeStorageOverlay(s, log.GetDefaultLogger())

	host := s.Drivers["local-host"]
	if v, _ := host["allowCreateMissing"].(bool); v {
		t.Errorf("operator-set allowCreateMissing=false should be preserved, got true")
	}
	allow, _ := host["hostPathAllowlist"].([]any)
	want := mustHomeRel(t)
	hasSrv := false
	hasDev := false
	for _, v := range allow {
		s, _ := v.(string)
		if s == "/srv/rune" {
			hasSrv = true
		}
		if s == want {
			hasDev = true
		}
	}
	if !hasSrv {
		t.Errorf("operator entry /srv/rune should remain in allowlist")
	}
	if !hasDev {
		t.Errorf("dev volume root %q should be appended to allowlist", want)
	}

	mgr := s.Drivers["local"]
	if got, _ := mgr["localVolumeRoot"].(string); got != "/var/custom/volumes" {
		t.Errorf("operator-set localVolumeRoot should be preserved, got %v", got)
	}
}

func TestApplyDevModeStorageOverlay_NoDuplicateAllowlistEntry(t *testing.T) {
	want := mustHomeRel(t)
	s := &config.Storage{
		Drivers: map[string]map[string]any{
			"local-host": {
				"hostPathAllowlist": []any{want},
			},
		},
	}
	applyDevModeStorageOverlay(s, log.GetDefaultLogger())

	allow, _ := s.Drivers["local-host"]["hostPathAllowlist"].([]any)
	count := 0
	for _, v := range allow {
		if s, ok := v.(string); ok && s == want {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected dev volume root to appear exactly once, got %d (%v)", count, allow)
	}
}

func mustHomeRel(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatalf("UserHomeDir: %v", err)
	}
	return filepath.Join(home, ".rune", "volumes")
}
