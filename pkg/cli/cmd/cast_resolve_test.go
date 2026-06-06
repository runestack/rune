package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/require"
)

// writeFile is a tiny test helper.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
}

func TestResolveCastSource_SingleLooseFile_DerivesName(t *testing.T) {
	dir := t.TempDir()
	svc := filepath.Join(dir, "postgres.yaml")
	writeFile(t, svc, "service:\n  name: postgres\n  image: postgres:16\n")

	rc, err := resolveCastSource([]string{svc}, &castOptions{})
	require.NoError(t, err)
	require.Equal(t, kindInline, rc.kind)
	require.Equal(t, "postgres", rc.releaseName, "name derives from file basename sans ext")
	require.True(t, rc.nameDerived, "derived name must be flagged for the warning")
	require.Equal(t, []string{svc}, rc.inlineFiles)
}

func TestResolveCastSource_ReleaseFlagOverridesDerived(t *testing.T) {
	dir := t.TempDir()
	svc := filepath.Join(dir, "postgres.yaml")
	writeFile(t, svc, "service:\n  name: postgres\n  image: postgres:16\n")

	rc, err := resolveCastSource([]string{svc}, &castOptions{releaseName: "db"})
	require.NoError(t, err)
	require.Equal(t, "db", rc.releaseName)
	require.False(t, rc.nameDerived, "explicit --release is not a derived name")
}

func TestResolveCastSource_LooseDir_DerivesDirName(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "my-app")
	writeFile(t, filepath.Join(app, "svc.yaml"), "service:\n  name: web\n  image: nginx\n")
	writeFile(t, filepath.Join(app, "cfg.yaml"), "configmap:\n  name: c\n  data:\n    k: v\n")

	rc, err := resolveCastSource([]string{app}, &castOptions{})
	require.NoError(t, err)
	require.Equal(t, kindInline, rc.kind)
	require.Equal(t, "my-app", rc.releaseName)
	require.True(t, rc.nameDerived)
}

func TestResolveCastSource_RunesetDir_UsesManifestName(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "rs")
	writeFile(t, filepath.Join(root, "runeset.yaml"), "name: webapp\nversion: v1\n")
	writeFile(t, filepath.Join(root, "casts", "svc.yaml"), "service:\n  name: web\n  image: nginx\n")

	rc, err := resolveCastSource([]string{root}, &castOptions{})
	require.NoError(t, err)
	require.Equal(t, kindRuneset, rc.kind)
	require.Equal(t, "webapp", rc.releaseName)
	require.False(t, rc.nameDerived)
	require.Equal(t, types.RunesetSourceTypeDirectory, rc.source.Type)
}

func TestResolveCastSource_AmbiguousDir_IsError(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "rs")
	// Both a runeset.yaml AND loose top-level manifests → ambiguous.
	writeFile(t, filepath.Join(root, "runeset.yaml"), "name: webapp\nversion: v1\n")
	writeFile(t, filepath.Join(root, "loose.yaml"), "service:\n  name: web\n  image: nginx\n")

	_, err := resolveCastSource([]string{root}, &castOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "both runeset.yaml and loose manifests")
}

func TestResolveCastSource_MultiArg_RequiresRelease(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.yaml")
	b := filepath.Join(dir, "b.yaml")
	writeFile(t, a, "service:\n  name: a\n  image: nginx\n")
	writeFile(t, b, "service:\n  name: b\n  image: nginx\n")

	_, err := resolveCastSource([]string{a, b}, &castOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "require an explicit --release")
}

func TestResolveCastSource_MultiArg_WithRelease(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.yaml")
	b := filepath.Join(dir, "b.yaml")
	writeFile(t, a, "service:\n  name: a\n  image: nginx\n")
	writeFile(t, b, "service:\n  name: b\n  image: nginx\n")

	rc, err := resolveCastSource([]string{a, b}, &castOptions{releaseName: "bundle"})
	require.NoError(t, err)
	require.Equal(t, kindInline, rc.kind)
	require.Equal(t, "bundle", rc.releaseName)
	require.False(t, rc.nameDerived)
	require.Len(t, rc.inlineFiles, 2)
}

func TestResolveCastSource_MultiArg_RejectsRunesetMix(t *testing.T) {
	dir := t.TempDir()
	loose := filepath.Join(dir, "a.yaml")
	writeFile(t, loose, "service:\n  name: a\n  image: nginx\n")
	rsRoot := filepath.Join(dir, "rs")
	writeFile(t, filepath.Join(rsRoot, "runeset.yaml"), "name: webapp\nversion: v1\n")

	_, err := resolveCastSource([]string{loose, rsRoot}, &castOptions{releaseName: "x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "runesets cannot be combined")
}

func TestResolveCastSource_NoArgsNoRuneset_Errors(t *testing.T) {
	// Run in a temp dir with no runeset.yaml.
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	require.NoError(t, os.Chdir(dir))
	defer func() { _ = os.Chdir(cwd) }()

	_, err := resolveCastSource(nil, &castOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no source given")
}

func TestResolveCastSource_MissingPath_Errors(t *testing.T) {
	_, err := resolveCastSource([]string{"/no/such/path/here.yaml"}, &castOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestDeriveNameFromPath(t *testing.T) {
	cases := map[string]string{
		"postgres.yaml":    "postgres",
		"db/postgres.yml":  "postgres",
		"my-app/":          "my-app",
		"./svc.YAML":       "svc",
		"config.prod.yaml": "config.prod",
	}
	for in, want := range cases {
		require.Equal(t, want, deriveNameFromPath(in), "deriveNameFromPath(%q)", in)
	}
}
