package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

// resolvedKind discriminates the two render strategies a resolved cast can take.
// Both flow through the same pipeline (resolve → render → plan → confirm →
// apply); they differ only in HOW rendered bytes are produced.
type resolvedKind int

const (
	// kindRuneset — a runeset.yaml-bearing source (directory, archive, github,
	// url). Rendered via the manifest + values/ + casts/ path.
	kindRuneset resolvedKind = iota
	// kindInline — one-or-more loose castfiles with no runeset.yaml. Rendered
	// as an "inline runeset": optional --values/--set templating, no manifest.
	kindInline
)

// resolvedCast is the single output of source resolution (§2, C2): it unifies
// the inline-vs-runeset fork into one shape carrying the release identity, its
// provenance, and the render strategy. Every `rune cast` produces exactly one
// of these; ambiguity is an error rather than a silent branch.
type resolvedCast struct {
	kind resolvedKind

	// releaseName is the resolved release identity. For runesets it is the
	// manifest name; for inline casts it is derived from the file/dir basename
	// (nameDerived=true) unless --release overrode it.
	releaseName string
	// nameDerived is true when releaseName was auto-chosen from a basename and
	// the user did not pin it with --release — surfaced as a warning (C2).
	nameDerived bool

	source types.ReleaseSource

	// runeset render inputs (kindRuneset).
	runesetRoot string

	// inline render inputs (kindInline): the expanded set of loose castfiles.
	inlineFiles []string

	// cleanup, when non-nil, removes any temp dir extracted for a remote/archive
	// runeset. The caller defers it.
	cleanup func()
}

// resolveCastSource is the one function that maps `rune cast <args>` plus flags
// to a single resolvedCast (CAST_REFACTOR_PLAN §2, decision C2).
//
// Precedence (deterministic, explicit errors on ambiguity):
//  1. no args + runeset.yaml in cwd        → directory runeset
//  2. <dir>/runeset.yaml                    → directory runeset
//  3. <dir> with BOTH runeset.yaml & loose  → ERROR (ambiguous)
//  4. *.runeset.tgz (file or https://)      → package/remote runeset
//  5. github.com/org/repo/path@ref          → github runeset
//  6. single loose .yaml file               → inline runeset, name = basename
//  7. <dir> of loose yamls (no runeset.yaml)→ inline runeset, name = dir base
//  8. multiple args                         → inline runeset, --release REQUIRED
//
// --release always overrides the derived name and is valid for every form.
func resolveCastSource(args []string, opts *castOptions) (*resolvedCast, error) {
	// Case 1: no args — only valid when cwd holds a runeset.yaml.
	if len(args) == 0 {
		if utils.FileExists("runeset.yaml") {
			return resolveRunesetDir(".", opts)
		}
		return nil, fmt.Errorf("no source given and no runeset.yaml in the current directory\n" +
			"hint: pass a cast file, a directory, a .runeset.tgz, or a github.com/org/repo path")
	}

	// Multiple args (case 8): always an inline runeset spanning all of them, and
	// --release is required since no single basename identifies the release.
	if len(args) > 1 {
		if opts.releaseName == "" {
			return nil, fmt.Errorf("multiple sources require an explicit --release name "+
				"(cannot derive one identity from %d arguments)", len(args))
		}
		// Reject mixing a runeset into a multi-arg inline cast — that is
		// ambiguous (which manifest wins?).
		for _, a := range args {
			if isRunesetArg(a) {
				return nil, fmt.Errorf("argument %q is a runeset; runesets cannot be combined with other sources in one cast", a)
			}
		}
		return resolveInlineFiles(args, opts.releaseName, false, opts)
	}

	arg := args[0]

	// Remote URL forms.
	if strings.HasPrefix(arg, "https://") {
		if strings.HasSuffix(strings.ToLower(arg), ".runeset.tgz") {
			return resolveRemoteArchive(arg, opts)
		}
		if strings.Contains(arg, "github.com/") {
			return resolveGitHub(arg, opts)
		}
		return nil, fmt.Errorf("unsupported https source %q: expected a .runeset.tgz archive or a github.com URL", arg)
	}

	// GitHub shorthand.
	if strings.HasPrefix(arg, "github.com/") {
		return resolveGitHub(arg, opts)
	}

	// Package archive on disk.
	if utils.FileExists(arg) && strings.HasSuffix(strings.ToLower(arg), ".runeset.tgz") {
		return resolvePackageArchive(arg, opts)
	}

	// Directory: could be a runeset, a loose-yaml dir, or ambiguously both.
	if utils.IsDirectory(arg) {
		hasManifest := utils.FileExists(filepath.Join(arg, "runeset.yaml"))
		hasLoose := dirHasLooseManifests(arg, opts.recursiveDir)

		switch {
		case hasManifest && hasLoose:
			// Ambiguity is a hard error, not a silent pick (C2).
			return nil, fmt.Errorf("directory %q contains both runeset.yaml and loose manifests; "+
				"pass one or the other (point at the runeset, or at a directory of loose casts)", arg)
		case hasManifest:
			return resolveRunesetDir(arg, opts)
		case hasLoose:
			name := opts.releaseName
			derived := false
			if name == "" {
				name = deriveNameFromPath(arg)
				derived = true
			}
			return resolveInlineFiles([]string{arg}, name, derived, opts)
		default:
			return nil, fmt.Errorf("directory %q contains no runeset.yaml and no .yaml/.yml manifests", arg)
		}
	}

	// Single loose file → inline runeset named after the file basename.
	if utils.FileExists(arg) {
		name := opts.releaseName
		derived := false
		if name == "" {
			name = deriveNameFromPath(arg)
			derived = true
		}
		return resolveInlineFiles([]string{arg}, name, derived, opts)
	}

	return nil, fmt.Errorf("source %q not found (not a file, directory, archive, or github path)", arg)
}

// dirHasLooseManifests reports whether a directory contains loose castfile
// YAMLs, EXCLUDING the runeset.yaml manifest itself (so a runeset directory is
// not mistaken for "also has loose manifests").
func dirHasLooseManifests(dir string, recursive bool) bool {
	yamls, err := utils.GetYAMLFilesInDirectory(dir, recursive)
	if err != nil {
		return false
	}
	for _, y := range yamls {
		if filepath.Base(y) == "runeset.yaml" {
			continue
		}
		return true
	}
	return false
}

// isRunesetArg reports whether a single argument denotes a runeset source
// (used to reject mixing runesets into a multi-arg inline cast).
func isRunesetArg(arg string) bool {
	if strings.HasPrefix(arg, "github.com/") {
		return true
	}
	if strings.HasPrefix(arg, "https://") {
		la := strings.ToLower(arg)
		return strings.HasSuffix(la, ".runeset.tgz") || strings.Contains(arg, "github.com/")
	}
	if strings.HasSuffix(strings.ToLower(arg), ".runeset.tgz") && utils.FileExists(arg) {
		return true
	}
	if utils.IsDirectory(arg) && utils.FileExists(filepath.Join(arg, "runeset.yaml")) {
		return true
	}
	return false
}

// deriveNameFromPath turns a file or directory path into a release name: the
// basename with any extension(s) stripped. "./db/postgres.yaml" → "postgres";
// "./my-app/" → "my-app".
func deriveNameFromPath(path string) string {
	base := filepath.Base(filepath.Clean(path))
	// Strip a trailing .yaml/.yml (single extension only — keep dotted names).
	for _, ext := range []string{".yaml", ".yml"} {
		if strings.HasSuffix(strings.ToLower(base), ext) {
			base = base[:len(base)-len(ext)]
			break
		}
	}
	if base == "" || base == "." || base == "/" {
		return "release"
	}
	return base
}

// --- per-form resolvers ---

func resolveRunesetDir(root string, opts *castOptions) (*resolvedCast, error) {
	mf, err := types.ParseRunesetManifest(filepath.Join(root, "runeset.yaml"))
	if err != nil {
		return nil, err
	}
	name := utils.PickFirstNonEmpty(opts.releaseName, mf.Name)
	if name == "" {
		return nil, fmt.Errorf("runeset %q has no manifest name; set 'name:' in runeset.yaml or pass --release", root)
	}
	return &resolvedCast{
		kind:        kindRuneset,
		releaseName: name,
		source: types.ReleaseSource{
			Type:     types.RunesetSourceTypeDirectory,
			Location: root,
		},
		runesetRoot: root,
	}, nil
}

func resolveRemoteArchive(url string, opts *castOptions) (*resolvedCast, error) {
	tmpFile, err := downloadRunesetArchive(url)
	if err != nil {
		return nil, err
	}
	tmpDir, err := extractRunesetArchive(tmpFile)
	_ = os.Remove(tmpFile)
	if err != nil {
		return nil, err
	}
	rc, err := resolveRunesetDir(tmpDir, opts)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	rc.source.Type = types.RunesetSourceTypeRemoteArchive
	rc.source.Location = url
	rc.cleanup = func() { _ = os.RemoveAll(tmpDir) }
	return rc, nil
}

func resolvePackageArchive(path string, opts *castOptions) (*resolvedCast, error) {
	tmpDir, err := extractRunesetArchive(path)
	if err != nil {
		return nil, err
	}
	rc, err := resolveRunesetDir(tmpDir, opts)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	rc.source.Type = types.RunesetSourceTypePackageArchive
	rc.source.Location = path
	rc.cleanup = func() { _ = os.RemoveAll(tmpDir) }
	return rc, nil
}

func resolveGitHub(ref string, opts *castOptions) (*resolvedCast, error) {
	root, err := resolveGitHubRuneset(ref)
	if err != nil {
		return nil, err
	}
	rc, err := resolveRunesetDir(root, opts)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	rc.source.Type = types.RunesetSourceTypeGitHub
	rc.source.Location = ref
	rc.source.Ref = githubRef(ref)
	rc.cleanup = func() { _ = os.RemoveAll(root) }
	return rc, nil
}

// resolveInlineFiles expands the given args into a concrete set of loose
// castfiles and builds an inline resolvedCast.
func resolveInlineFiles(args []string, name string, derived bool, opts *castOptions) (*resolvedCast, error) {
	files, err := utils.ExpandFilePaths(args, opts.recursiveDir)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no cast files found in %s", strings.Join(args, ", "))
	}
	return &resolvedCast{
		kind:        kindInline,
		releaseName: name,
		nameDerived: derived,
		source: types.ReleaseSource{
			Type:     types.RunesetSourceTypeDirectory, // inline casts have no archive provenance
			Location: strings.Join(args, ","),
		},
		inlineFiles: files,
	}, nil
}

// githubRef extracts the ref portion (after @) of a github shorthand, defaulting
// to main — mirrors resolveGitHubRuneset's parsing so the recorded source is
// reproducible.
func githubRef(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 && i+1 < len(ref) {
		return ref[i+1:]
	}
	return "main"
}
