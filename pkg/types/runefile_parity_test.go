package types_test

// RUNE-112: parity test between the two parallel runefile schemas.
//
//   pkg/types.RuneFile     used by `rune lint` (operator-facing)
//   internal/config.Config used by `runed`     (server runtime)
//
// They are maintained by hand and have drifted twice in the project's
// history (registries gap → RUNE-111, secret.limits, etc.). This test
// makes the next drift loud by walking both structs reflectively and
// asserting their top-level yaml-tag sets agree, plus a handful of
// pinpoint assertions for fields that actually slipped before.
//
// When this test fails: add the missing field to whichever struct is
// behind, or — if the asymmetry is intentional (e.g. a runed-only
// internal field) — add the path to one of the explicit allowlists
// below with a comment explaining why.

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/types"
)

// runefileExtras are top-level yaml keys legitimately present on
// pkg/types.RuneFile that are NOT struct fields on internal/config.Config.
//
// Empty today. Both structs declare the full schema. (runed itself
// still reads networking/telemetry/node/ingress/acme via
// viper.GetString in cmd/runed/main.go rather than through Config —
// see the doc-comments on those types in internal/config — but the
// field declarations exist so this test stays a real bidirectional
// check.)
var runefileExtras = map[string]string{}

// configExtras are top-level yaml keys legitimately present on
// internal/config.Config that are NOT mirrored on pkg/types.RuneFile.
// Empty today — kept here as a hook for future intentional asymmetry.
var configExtras = map[string]string{}

func topLevelYAMLTags(t *testing.T, v any) []string {
	t.Helper()
	rt := reflect.TypeOf(v)
	if rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct {
		t.Fatalf("topLevelYAMLTags: expected struct, got %s", rt.Kind())
	}
	var tags []string
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.SplitN(tag, ",", 2)[0]
		if name == "" {
			continue
		}
		tags = append(tags, name)
	}
	sort.Strings(tags)
	return tags
}

func TestRuneFileParityTopLevelKeys(t *testing.T) {
	rfTags := topLevelYAMLTags(t, types.RuneFile{})
	cfgTags := topLevelYAMLTags(t, config.Config{})

	rfSet := map[string]bool{}
	for _, k := range rfTags {
		rfSet[k] = true
	}
	cfgSet := map[string]bool{}
	for _, k := range cfgTags {
		cfgSet[k] = true
	}

	// Every key on Config must appear on RuneFile (or be in the
	// explicit configExtras allowlist). This is the drift direction
	// we care about most: runed grew a field, lint hasn't caught up.
	for _, k := range cfgTags {
		if rfSet[k] {
			continue
		}
		if _, ok := configExtras[k]; ok {
			continue
		}
		t.Errorf("internal/config.Config has top-level key %q that pkg/types.RuneFile is missing — add it to RuneFile (and to isKnownField) or whitelist in configExtras", k)
	}

	// Every key on RuneFile must appear on Config (or be in
	// runefileExtras for the runed-via-viper-direct cases).
	for _, k := range rfTags {
		if cfgSet[k] {
			continue
		}
		if _, ok := runefileExtras[k]; ok {
			continue
		}
		t.Errorf("pkg/types.RuneFile has top-level key %q that internal/config.Config is missing — add it to Config or whitelist in runefileExtras", k)
	}
}

// TestRuneFileParityRegistries pins the specific drift that triggered
// RUNE-111: docker.registries was on Config but not on RuneFile, so
// every TF-rendered runefile in production tripped lint.
func TestRuneFileParityRegistries(t *testing.T) {
	dt := reflect.TypeOf(types.DockerConfig{})
	if _, ok := dt.FieldByNameFunc(func(name string) bool {
		f, _ := dt.FieldByName(name)
		return strings.SplitN(f.Tag.Get("yaml"), ",", 2)[0] == "registries"
	}); !ok {
		t.Fatalf("pkg/types.DockerConfig is missing the `registries` field — this is the RUNE-111 regression")
	}

	at := reflect.TypeOf(types.DockerRegistryAuth{})
	required := []string{"type", "username", "password", "token", "region", "fromSecret", "bootstrap", "manage", "immutable", "data"}
	have := map[string]bool{}
	for i := 0; i < at.NumField(); i++ {
		name := strings.SplitN(at.Field(i).Tag.Get("yaml"), ",", 2)[0]
		have[name] = true
	}
	for _, r := range required {
		if !have[r] {
			t.Errorf("pkg/types.DockerRegistryAuth is missing yaml field %q (required to lint runefiles produced by terraform-digitalocean-rune)", r)
		}
	}
}
