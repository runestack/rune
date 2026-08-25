package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/release"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

// renderedRelease is the output of rendering a resolvedCast: the desired
// resource ref set, the rendered payloads keyed by OwnerRef.Key(), the merged
// values, the manifest, and a digest of the rendered bytes. This is the
// client-side product that becomes a ReleaseSpec + payloads for the Cast RPC
// (C1: client renders, server reconciles).
type renderedRelease struct {
	namespace string
	manifest  types.RunesetManifest
	values    map[string]interface{}
	digest    string

	resources []release.DesiredResource
	payloads  castPayloads
}

// castPayloads mirrors releasectl.Payloads but is built client-side. Keyed by
// OwnerRef.Key().
type castPayloads struct {
	services   map[string]*types.Service
	secrets    map[string]*types.Secret
	configmaps map[string]*types.Configmap
	volumes    map[string]*types.Volume
}

func newCastPayloads() castPayloads {
	return castPayloads{
		services:   map[string]*types.Service{},
		secrets:    map[string]*types.Secret{},
		configmaps: map[string]*types.Configmap{},
		volumes:    map[string]*types.Volume{},
	}
}

// lint validates the FULLY-RENDERED resources (templates already resolved, so
// names are final) before planning. This makes malformed manifests — e.g. an
// invalid DNS-1123 name — fail fast with a clear error, instead of passing the
// plan and failing part-way through apply server-side. Errors are aggregated and
// sorted for deterministic output.
func (r *renderedRelease) lint() error {
	var errs []string
	check := func(kind, namespace, name string) {
		if name == "" {
			errs = append(errs, fmt.Sprintf("%s in namespace %q has an empty name", kind, namespace))
			return
		}
		if err := utils.ValidateDNS1123Name(name); err != nil {
			errs = append(errs, fmt.Sprintf("%s %q: invalid name: %v", kind, name, err))
		}
		if namespace != "" {
			if err := utils.ValidateDNS1123Name(namespace); err != nil {
				errs = append(errs, fmt.Sprintf("%s %q: invalid namespace %q: %v", kind, name, namespace, err))
			}
		}
	}
	for _, s := range r.payloads.services {
		check("service", s.Namespace, s.Name)
		// Services carry a richer schema; reuse the same Validate the server runs
		// at create time so we fail fast on it too.
		if s.Name != "" {
			if err := s.Validate(); err != nil {
				errs = append(errs, fmt.Sprintf("service %q: %v", s.Name, err))
			}
		}
	}
	for _, s := range r.payloads.secrets {
		check("secret", s.Namespace, s.Name)
	}
	for _, c := range r.payloads.configmaps {
		check("configmap", c.Namespace, c.Name)
	}
	for _, v := range r.payloads.volumes {
		check("volume", v.Namespace, v.Name)
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("rendered cast has invalid resource(s):\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// updateWarnings returns the advisory update findings for the rendered
// services, sorted so repeated casts print the same order. These never fail a
// cast — they exist because `rune lint` is opt-in and nobody runs it before a
// deploy that looks fine.
// capacityWarnings reports requests no known node can satisfy. Ordered
// like updateWarnings so the advisory block is stable across runs.
func (r *renderedRelease) capacityWarnings(alloc *nodeAllocatable) []string {
	keys := make([]string, 0, len(r.payloads.services))
	for k := range r.payloads.services {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	svcs := make([]*types.Service, 0, len(keys))
	for _, k := range keys {
		svcs = append(svcs, r.payloads.services[k])
	}
	return capacityWarnings(svcs, alloc)
}

func (r *renderedRelease) updateWarnings() []string {
	keys := make([]string, 0, len(r.payloads.services))
	for k := range r.payloads.services {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var warns []string
	for _, k := range keys {
		s := r.payloads.services[k]
		if s == nil {
			continue
		}
		// Sort services, not findings: UpdateWarnings orders its rules by how
		// much they should change your mind, and that order is worth keeping.
		for _, w := range s.UpdateWarnings() {
			warns = append(warns, fmt.Sprintf("%s: %s", s.Name, w))
		}
	}
	return warns
}

// renderResolvedCast renders a resolvedCast into a renderedRelease. It reuses
// the existing render + castfile-parse logic (runeset path for kindRuneset, the
// inline render-bytes path for kindInline) so behavior matches today, but emits
// a normalized resource set instead of driving a per-resource apply loop.
func renderResolvedCast(rc *resolvedCast, opts *castOptions) (*renderedRelease, error) {
	switch rc.kind {
	case kindRuneset:
		return renderRunesetRelease(rc, opts)
	case kindInline:
		return renderInlineRelease(rc, opts)
	default:
		return nil, fmt.Errorf("unknown resolved cast kind")
	}
}

// renderCastBytes returns the raw rendered castfile bytes for a resolvedCast,
// used by the --render path (print without applying). Mirrors the render inputs
// of renderResolvedCast but skips resource extraction.
func renderCastBytes(rc *resolvedCast, opts *castOptions) ([][]byte, error) {
	switch rc.kind {
	case kindRuneset:
		root := rc.runesetRoot
		mf, err := types.ParseRunesetManifest(filepath.Join(root, "runeset.yaml"))
		if err != nil {
			return nil, err
		}
		ctx := buildContextFromManifest(mf, opts)
		ctx.ReleaseName = rc.releaseName
		mergedValues, err := mergeRunesetValuesTemplated(mf, root, ctx.GetValues(), opts)
		if err != nil {
			return nil, err
		}
		castFiles, err := gatherRunesetCasts(filepath.Join(root, "casts"))
		if err != nil {
			return nil, err
		}
		return renderRunesetCasts(root, castFiles, mergedValues, ctx.GetValues())
	case kindInline:
		values, err := mergeCastFileValues(opts)
		if err != nil {
			return nil, err
		}
		var out [][]byte
		for _, fp := range rc.inlineFiles {
			raw, readErr := os.ReadFile(fp)
			if readErr != nil {
				return nil, fmt.Errorf("failed to read file %s: %w", fp, readErr)
			}
			b, renderErr := renderCastFileBytes(fp, raw, values, opts.namespace, castMode(opts))
			if renderErr != nil {
				return nil, renderErr
			}
			out = append(out, b)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown resolved cast kind")
	}
}

// renderRunesetRelease renders a runeset directory into a renderedRelease,
// reusing the manifest/values/casts pipeline from cast_runeset.go.
func renderRunesetRelease(rc *resolvedCast, opts *castOptions) (*renderedRelease, error) {
	root := rc.runesetRoot
	mf, err := types.ParseRunesetManifest(filepath.Join(root, "runeset.yaml"))
	if err != nil {
		return nil, err
	}
	// Honor --release for the {{ releaseName }} template too.
	ctx := buildContextFromManifest(mf, opts)
	ctx.ReleaseName = rc.releaseName

	castsDir := filepath.Join(root, "casts")
	if !utils.IsDirectory(castsDir) {
		return nil, fmt.Errorf("runeset %s missing required 'casts/' directory", root)
	}
	mergedValues, err := mergeRunesetValuesTemplated(mf, root, ctx.GetValues(), opts)
	if err != nil {
		return nil, err
	}
	castFiles, err := gatherRunesetCasts(castsDir)
	if err != nil {
		return nil, err
	}
	rendered, err := renderRunesetCasts(root, castFiles, mergedValues, ctx.GetValues())
	if err != nil {
		return nil, err
	}

	out := &renderedRelease{
		namespace: ctx.Namespace,
		manifest:  mf,
		values:    mergedValues,
		payloads:  newCastPayloads(),
	}
	names := make([]string, len(castFiles))
	for i := range castFiles {
		names[i] = filepath.Base(castFiles[i])
	}
	if err := collectRenderedCasts(out, rendered, names, ctx.Namespace); err != nil {
		return nil, err
	}
	out.digest = digestRendered(rendered)
	return out, nil
}

// renderInlineRelease renders one-or-more loose castfiles into a renderedRelease
// (no manifest, optional --values/--set templating), reusing
// renderCastFileBytes from cast_runeset.go.
func renderInlineRelease(rc *resolvedCast, opts *castOptions) (*renderedRelease, error) {
	ns := opts.namespace
	values, err := mergeCastFileValues(opts)
	if err != nil {
		return nil, err
	}

	out := &renderedRelease{
		namespace: ns,
		values:    values,
		payloads:  newCastPayloads(),
	}
	var rendered [][]byte
	var names []string
	for _, fp := range rc.inlineFiles {
		raw, readErr := os.ReadFile(fp)
		if readErr != nil {
			return nil, fmt.Errorf("failed to read file %s: %w", fp, readErr)
		}
		b, renderErr := renderCastFileBytes(fp, raw, values, ns, castMode(opts))
		if renderErr != nil {
			return nil, renderErr
		}
		rendered = append(rendered, b)
		names = append(names, filepath.Base(fp))
	}
	if err := collectRenderedCasts(out, rendered, names, ns); err != nil {
		return nil, err
	}
	out.digest = digestRendered(rendered)
	return out, nil
}

// collectRenderedCasts parses each rendered castfile, lints it, and extracts the
// concrete resources into the renderedRelease's desired-ref set and payloads.
// Lint errors are aggregated and returned together (line-numbered, per file).
func collectRenderedCasts(out *renderedRelease, rendered [][]byte, names []string, namespace string) error {
	var lintErrs []string
	for i, b := range rendered {
		name := names[i]
		cf, err := types.ParseCastFileFromBytes(b, namespace)
		if err != nil {
			return fmt.Errorf("failed to parse %s: %w", name, err)
		}
		if errs := cf.Lint(); len(errs) > 0 {
			for _, le := range errs {
				lintErrs = append(lintErrs, fmt.Sprintf("%s: %s", name, le.Error()))
			}
			continue
		}
		if err := extractResources(out, cf, name); err != nil {
			return err
		}
	}
	if len(lintErrs) > 0 {
		return fmt.Errorf("validation failed:\n  - %s", strings.Join(lintErrs, "\n  - "))
	}
	// Deterministic ordering of the desired set (the server orders applies, but a
	// stable client digest/plan is nicer): sort by ref key.
	sort.SliceStable(out.resources, func(i, j int) bool {
		return out.resources[i].Ref.Key() < out.resources[j].Ref.Key()
	})
	return nil
}

// extractResources pulls services/secrets/configmaps/volumes/storageclasses out
// of a castfile, appending a DesiredResource per item and stashing the rendered
// payload keyed by OwnerRef.Key(). Volumes that name a StorageClass also record
// a reference to that shared kind (D2: referenced, never owned).
func extractResources(out *renderedRelease, cf *types.CastFile, fileName string) error {
	svcs, err := cf.GetServices()
	if err != nil {
		return fmt.Errorf("%s: extract services: %w", fileName, err)
	}
	for _, s := range svcs {
		ref := refFor(types.ResourceTypeService, s.Namespace, s.Name, out.namespace)
		out.resources = append(out.resources, release.DesiredResource{Ref: ref})
		out.payloads.services[ref.Key()] = s
	}

	secs, err := cf.GetSecrets()
	if err != nil {
		return fmt.Errorf("%s: extract secrets: %w", fileName, err)
	}
	for _, s := range secs {
		ref := refFor(types.ResourceTypeSecret, s.Namespace, s.Name, out.namespace)
		out.resources = append(out.resources, release.DesiredResource{Ref: ref})
		out.payloads.secrets[ref.Key()] = s
	}

	cfgs, err := cf.GetConfigmaps()
	if err != nil {
		return fmt.Errorf("%s: extract configmaps: %w", fileName, err)
	}
	for _, c := range cfgs {
		ref := refFor(types.ResourceTypeConfigmap, c.Namespace, c.Name, out.namespace)
		out.resources = append(out.resources, release.DesiredResource{Ref: ref})
		out.payloads.configmaps[ref.Key()] = c
	}

	scs, err := cf.GetStorageClasses()
	if err != nil {
		return fmt.Errorf("%s: extract storage classes: %w", fileName, err)
	}
	for _, sc := range scs {
		// StorageClass is a shared cluster-scoped kind: referenced, never owned
		// (D2). The planner treats it as ActionReference; no payload is applied
		// through the release path.
		ref := refFor(types.ResourceTypeStorageClass, "", sc.Name, "")
		out.resources = append(out.resources, release.DesiredResource{Ref: ref})
	}

	vols, err := cf.GetVolumes()
	if err != nil {
		return fmt.Errorf("%s: extract volumes: %w", fileName, err)
	}
	for _, v := range vols {
		ref := refFor(types.ResourceTypeVolume, v.Namespace, v.Name, out.namespace)
		out.resources = append(out.resources, release.DesiredResource{Ref: ref})
		out.payloads.volumes[ref.Key()] = v
	}
	return nil
}

// resolveSecretTemplates renders cast-time `{{ secret:... }}` placeholders in the
// release's secret payloads (RUNE-105), in-place. Components defined in the same
// release are resolved in topo order; out-of-release components are revealed via
// the API. apiClient may be nil when no secret references an out-of-release
// component (the render path stays offline in that case).
func (r *renderedRelease) resolveSecretTemplates(apiClient *client.Client) error {
	if len(r.payloads.secrets) == 0 {
		return nil
	}
	info := &ResourceInfo{SecretsByFile: map[string][]*types.Secret{}}
	secrets := make([]*types.Secret, 0, len(r.payloads.secrets))
	for _, s := range r.payloads.secrets {
		secrets = append(secrets, s)
	}
	info.SecretsByFile["release"] = secrets
	return renderSecretTemplates(apiClient, info)
}

// refFor builds an OwnerRef, falling back to the release namespace when the
// resource left its namespace blank (cluster-scoped kinds pass "").
func refFor(rt types.ResourceType, ns, name, fallbackNS string) types.OwnerRef {
	if ns == "" {
		ns = fallbackNS
	}
	return types.OwnerRef{ResourceType: rt, Namespace: ns, Name: name}
}

// digestRendered returns a stable checksum of the rendered castfile set, used as
// the release's RenderedDigest (drift baseline). Order-independent: each
// rendered block is hashed and the per-block hashes are sorted before the final
// roll-up so file iteration order never changes the digest.
func digestRendered(rendered [][]byte) string {
	blocks := make([]string, 0, len(rendered))
	for _, b := range rendered {
		h := sha256.Sum256(b)
		blocks = append(blocks, hex.EncodeToString(h[:]))
	}
	sort.Strings(blocks)
	final := sha256.Sum256([]byte(strings.Join(blocks, "\n")))
	return hex.EncodeToString(final[:])
}

// toReleaseSpec converts a renderedRelease into the pure-core release.ReleaseSpec
// the client ships to the server (refs + identity + provenance + flags).
func (r *renderedRelease) toReleaseSpec(name string, src types.ReleaseSource, opts *castOptions) release.ReleaseSpec {
	return release.ReleaseSpec{
		Name:           name,
		Namespace:      r.namespace,
		Source:         src,
		Manifest:       r.manifest,
		Values:         r.values,
		RenderedDigest: r.digest,
		Resources:      r.resources,
		Options:        release.Options{Adopt: opts.adopt},
		Detach:         opts.detach,
		Atomic:         opts.atomic,
	}
}

// totalResources counts every desired resource (for display).
func (r *renderedRelease) totalResources() int { return len(r.resources) }

// printUpdateWarnings renders advisory findings under the plan block. Same
// glyph and shape as `rune lint` so the two read as one thing.
func printUpdateWarnings(w io.Writer, warns []string) {
	if len(warns) == 0 {
		return
	}
	for _, warn := range warns {
		fmt.Fprintf(w, "  %s %s\n", format.Dim("⚠"), warn)
	}
	fmt.Fprintln(w)
}
