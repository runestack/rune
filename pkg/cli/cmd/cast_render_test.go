package cmd

import (
	"path/filepath"
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/require"
)

// optsNS sets the embedded namespace on a castOptions and returns it (namespace
// lives in the embedded cmdOptions, so it can't be set in a struct literal).
func optsNS(ns string, o *castOptions) *castOptions {
	o.namespace = ns
	return o
}

func TestRenderResolvedCast_Inline_BuildsRefsAndPayloads(t *testing.T) {
	dir := t.TempDir()
	svc := filepath.Join(dir, "web.yaml")
	writeFile(t, svc, "service:\n  name: web\n  namespace: default\n  image: nginx:1\n  scale: 1\n")
	cfg := filepath.Join(dir, "cfg.yaml")
	writeFile(t, cfg, "configmap:\n  name: settings\n  namespace: default\n  data:\n    k: v\n")

	opts := &castOptions{}
	opts.namespace = "default"
	rc, err := resolveCastSource([]string{svc, cfg}, optsNS("default", &castOptions{releaseName: "app"}))
	require.NoError(t, err)

	rendered, err := renderResolvedCast(rc, opts)
	require.NoError(t, err)
	require.Equal(t, 2, rendered.totalResources())

	// Desired refs are sorted by key and carry the right types.
	var haveSvc, haveCfg bool
	for _, d := range rendered.resources {
		switch d.Ref.ResourceType {
		case types.ResourceTypeService:
			haveSvc = true
			require.Equal(t, "web", d.Ref.Name)
			require.NotNil(t, rendered.payloads.services[d.Ref.Key()])
		case types.ResourceTypeConfigmap:
			haveCfg = true
			require.Equal(t, "settings", d.Ref.Name)
			require.NotNil(t, rendered.payloads.configmaps[d.Ref.Key()])
		}
	}
	require.True(t, haveSvc && haveCfg)
	require.NotEmpty(t, rendered.digest)
}

func TestRenderResolvedCast_Inline_NamespaceFallback(t *testing.T) {
	dir := t.TempDir()
	svc := filepath.Join(dir, "web.yaml")
	// No namespace in the manifest → falls back to the cast namespace.
	writeFile(t, svc, "service:\n  name: web\n  image: nginx:1\n  scale: 1\n")

	opts := &castOptions{}
	opts.namespace = "prod"
	rc, err := resolveCastSource([]string{svc}, optsNS("prod", &castOptions{}))
	require.NoError(t, err)

	rendered, err := renderResolvedCast(rc, opts)
	require.NoError(t, err)
	require.Len(t, rendered.resources, 1)
	require.Equal(t, "prod", rendered.resources[0].Ref.Namespace)
}

func TestRenderResolvedCast_Digest_OrderIndependent(t *testing.T) {
	// Two files in either order must yield the same digest.
	dir := t.TempDir()
	a := filepath.Join(dir, "a.yaml")
	b := filepath.Join(dir, "b.yaml")
	writeFile(t, a, "service:\n  name: a\n  namespace: default\n  image: nginx\n  scale: 1\n")
	writeFile(t, b, "service:\n  name: b\n  namespace: default\n  image: nginx\n  scale: 1\n")

	opts := &castOptions{}
	opts.namespace = "default"

	rc1, err := resolveCastSource([]string{a, b}, optsNS("default", &castOptions{releaseName: "x"}))
	require.NoError(t, err)
	r1, err := renderResolvedCast(rc1, opts)
	require.NoError(t, err)

	rc2, err := resolveCastSource([]string{b, a}, optsNS("default", &castOptions{releaseName: "x"}))
	require.NoError(t, err)
	r2, err := renderResolvedCast(rc2, opts)
	require.NoError(t, err)

	require.Equal(t, r1.digest, r2.digest, "digest must be order-independent")
}

func TestToReleaseSpec_CarriesAdoptAndDetach(t *testing.T) {
	dir := t.TempDir()
	svc := filepath.Join(dir, "web.yaml")
	writeFile(t, svc, "service:\n  name: web\n  namespace: default\n  image: nginx\n  scale: 1\n")
	opts := &castOptions{adopt: true, detach: true}
	opts.namespace = "default"
	rc, err := resolveCastSource([]string{svc}, optsNS("default", &castOptions{}))
	require.NoError(t, err)
	rendered, err := renderResolvedCast(rc, opts)
	require.NoError(t, err)

	spec := rendered.toReleaseSpec("web", rc.source, opts)
	require.True(t, spec.Options.Adopt)
	require.True(t, spec.Detach)
	require.Equal(t, "web", spec.Name)
}
