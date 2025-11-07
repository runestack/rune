package cmd

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	yaml "gopkg.in/yaml.v3"
)

func runRunesetCast(root string, opts *castOptions) error {
	startTime := time.Now()

	// Read manifest
	manifestPath := filepath.Join(root, "runeset.yaml")
	mf, err := types.ParseRunesetManifest(manifestPath)
	if err != nil {
		return err
	}

	castsDir := filepath.Join(root, "casts")
	if !utils.IsDirectory(castsDir) {
		return fmt.Errorf("runeset %s missing required 'casts/' directory", root)
	}

	// Build execution context map (read-only)
	ctx := buildContextFromManifest(mf, opts)

	// Merge runeset values (collect first, resolve later)
	mergedValues, err := mergeRunesetValuesTemplated(mf, root, ctx.GetValues(), opts)
	if err != nil {
		return err
	}

	// Gather casts
	castFiles, err := gatherRunesetCasts(castsDir)
	if err != nil {
		return err
	}

	// Render casts
	rendered, err := renderRunesetCasts(root, castFiles, mergedValues, ctx.GetValues())
	if err != nil {
		return err
	}

	if opts.renderOnly {
		for _, b := range rendered {
			fmt.Println(string(b))
			fmt.Println("---")
		}
		return nil
	}

	// Build ResourceInfo from rendered casts
	info := &ResourceInfo{
		FilesByType:      make(map[string][]string),
		ServicesByFile:   make(map[string][]*types.Service),
		SecretsByFile:    make(map[string][]*types.Secret),
		ConfigmapsByFile: make(map[string][]*types.Configmap),
		TotalResources:   0,
		SourceArguments:  []string{root},
	}

	fmt.Println("🧩 Rendering and validating runeset casts...")
	var errorMessages []string
	for i, b := range rendered {
		fileName := filepath.Base(castFiles[i])
		fmt.Printf("- %-20s", fileName)
		dotsNeeded := 35 - (2 + len(fileName))
		if dotsNeeded < 3 {
			dotsNeeded = 3
		}
		fmt.Print(strings.Repeat(".", dotsNeeded))
		fmt.Print(" ")

		cf, err := types.ParseCastFileFromBytes(b, ctx.Namespace)
		if err != nil {
			fmt.Println("❌")
			return fmt.Errorf("failed to parse rendered YAML %s: %w", fileName, err)
		}
		if lintErrs := cf.Lint(); len(lintErrs) > 0 {
			fmt.Println("❌")
			for _, le := range lintErrs {
				errorMessages = append(errorMessages, fmt.Sprintf("%s: %s", fileName, le.Error()))
			}
			continue
		}
		fmt.Println("✓")

		if svcs, err := cf.GetServices(); err != nil {
			errorMessages = append(errorMessages, fmt.Sprintf("%s: failed to extract services: %v", fileName, err))
		} else if len(svcs) > 0 {
			info.ServicesByFile[fileName] = svcs
			info.TotalResources += len(svcs)
			info.FilesByType["Service"] = append(info.FilesByType["Service"], fileName)
		}
		if secs, err := cf.GetSecrets(); err != nil {
			errorMessages = append(errorMessages, fmt.Sprintf("%s: failed to extract secrets: %v", fileName, err))
		} else if len(secs) > 0 {
			info.SecretsByFile[fileName] = secs
			info.TotalResources += len(secs)
			info.FilesByType["Secret"] = append(info.FilesByType["Secret"], fileName)
		}
		if cfgs, err := cf.GetConfigmaps(); err != nil {
			errorMessages = append(errorMessages, fmt.Sprintf("%s: failed to extract configmaps: %v", fileName, err))
		} else if len(cfgs) > 0 {
			info.ConfigmapsByFile[fileName] = cfgs
			info.TotalResources += len(cfgs)
			info.FilesByType["Configmap"] = append(info.FilesByType["Configmap"], fileName)
		}
	}
	if len(errorMessages) > 0 {
		fmt.Println("❌ Validation errors:")
		for _, m := range errorMessages {
			fmt.Printf("  - %s\n", m)
		}
		return fmt.Errorf("validation failed for one or more rendered casts")
	}

	// Print banner and resource info
	printCastBanner([]string{root}, opts.detach)
	printResourceInfo(info, opts)

	// If dry-run only
	if opts.dryRun {
		fmt.Println("✅ Validation successful!")
		fmt.Println("💬 Use without --dry-run to deploy.")
		return nil
	}

	// Create API client
	apiClient, err := newAPIClient("", "")
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	fmt.Println("🚀 Preparing deployment plan...")

	deploymentResults, err := deployResources(apiClient, info, mustParseDuration(opts.timeoutStr), opts)
	if err != nil {
		return err
	}
	if opts.detach {
		printDetachedModeSummary(deploymentResults, startTime)
	} else {
		printWatchModeSummary(deploymentResults, startTime, opts)
	}
	return nil
}

func mustParseDuration(s string) time.Duration {
	d, _ := time.ParseDuration(s)
	return d
}

// mergeInto merges src into dst (map[string]interface{}), overwriting on conflict.
func mergeInto(dst, src map[string]interface{}) {
	if src == nil {
		return
	}
	for k, v := range src {
		if mv, ok := v.(map[string]interface{}); ok {
			if existing, ok2 := dst[k].(map[string]interface{}); ok2 {
				mergeInto(existing, mv)
				dsf := existing
				dst[k] = dsf
			} else {
				dst[k] = cloneMap(mv)
			}
			continue
		}
		dst[k] = v
	}
}

func cloneMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		if cm, ok := v.(map[string]interface{}); ok {
			out[k] = cloneMap(cm)
		} else {
			out[k] = v
		}
	}
	return out
}

// applySetValues applies key=value pairs to a nested map using dot-notation keys.
func applySetValues(target map[string]interface{}, sets []string) error {
	for _, s := range sets {
		parts := strings.SplitN(s, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid --set entry %q, expected key=value", s)
		}
		keyPath := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		setNestedValue(target, keyPath, val)
	}
	return nil
}

func setNestedValue(target map[string]interface{}, path string, val interface{}) {
	segments := strings.Split(path, ".")
	m := target
	for i := 0; i < len(segments)-1; i++ {
		seg := segments[i]
		child, ok := m[seg]
		if !ok {
			child = make(map[string]interface{})
			m[seg] = child
		}
		cm, ok := child.(map[string]interface{})
		if !ok {
			cm = make(map[string]interface{})
			m[seg] = cm
		}
		m = cm
	}
	m[segments[len(segments)-1]] = val
}

// readEnvValues reads environment variables with prefix RUNE_VALUES_ and converts them
// into a nested map using double underscore (__) to denote nesting, and single underscore
// for keys (e.g., RUNE_VALUES_app__env__LOG_LEVEL=info -> values.app.env.LOG_LEVEL=info)
func readEnvValues() map[string]interface{} {
	out := make(map[string]interface{})
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "RUNE_VALUES_") {
			continue
		}
		kv := strings.SplitN(strings.TrimPrefix(e, "RUNE_VALUES_"), "=", 2)
		if len(kv) != 2 {
			continue
		}
		keyPart := kv[0]
		val := kv[1]
		// Convert __ to dot for nesting
		path := strings.ReplaceAll(keyPart, "__", ".")
		// Lowercase top-level segment names like app; keep exact key for leafs
		setNestedValue(out, path, val)
	}
	return out
}

// mergeRunesetValues merges values from runeset manifest, values dir, env, values files,
// and --set flags (in order).
func mergeRunesetValues(mf types.RunesetManifest, root string, opts *castOptions) (map[string]interface{}, error) {
	merged := make(map[string]interface{})
	mergeInto(merged, mf.Defaults)

	// values directory (optional): merge all *.yaml/*.yml in lexicographic order
	if err := mergeValuesFromDir(filepath.Join(root, "values"), merged); err != nil {
		return nil, err
	}
	mergeInto(merged, readEnvValues())
	for _, vf := range opts.valuesFiles {
		b, err := os.ReadFile(vf)
		if err != nil {
			return nil, fmt.Errorf("failed to read values file %s: %w", vf, err)
		}
		// Pre-quote unquoted placeholders to avoid YAML parse errors
		text := utils.PreQuoteUnquotedPlaceholders(string(b))
		var m map[string]interface{}
		if err := yaml.Unmarshal([]byte(text), &m); err != nil {
			return nil, fmt.Errorf("failed to parse values file %s: %w", vf, err)
		}
		mergeInto(merged, m)
	}
	if err := applySetValues(merged, opts.setValues); err != nil {
		return nil, err
	}
	return merged, nil
}

// mergeRunesetValuesTemplated collects all values plainly, then resolves templated strings
// using the fully merged map. This requires templated scalars in YAML files to be quoted.
func mergeRunesetValuesTemplated(mf types.RunesetManifest, root string, ctx map[string]interface{}, opts *castOptions) (map[string]interface{}, error) {
	mv, err := mergeRunesetValues(mf, root, opts)
	if err != nil {
		return nil, err
	}
	// 7) Final resolve pass over merged map (handles remaining templated strings)
	r := utils.NewRenderer(root, mv, ctx)
	if err := r.RenderValuesMapFixpoint(mv, 3); err != nil {
		return nil, err
	}
	return mv, nil
}

// mergeValuesFromDir merges all YAML files in a directory into target in lexicographic order.
func mergeValuesFromDir(dir string, target map[string]interface{}) error {
	if !utils.IsDirectory(dir) {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("failed to read values file %s: %w", f, err)
		}
		// Pre-quote unquoted placeholders to avoid YAML parse errors
		text := utils.PreQuoteUnquotedPlaceholders(string(b))
		var m map[string]interface{}
		if err := yaml.Unmarshal([]byte(text), &m); err != nil {
			return fmt.Errorf("failed to parse values file %s: %w", f, err)
		}
		mergeInto(target, m)
	}
	return nil
}

// buildContextFromManifest constructs the context map used by renderer from manifest and flags.
func buildContextFromManifest(mf types.RunesetManifest, opts *castOptions) types.RunesetContext {
	// effective namespace
	effectiveNS := utils.PickFirstNonEmpty(opts.namespace, mf.Namespace)
	release := utils.PickFirstNonEmpty(opts.releaseName, mf.Name)

	// Build execution context map (read-only)
	mode := "apply"
	if opts.renderOnly {
		mode = "render"
	} else if opts.dryRun {
		mode = "dry-run"
	}

	// Build context map
	return types.RunesetContext{
		Namespace:   effectiveNS,
		Mode:        mode,
		ReleaseName: release,
		Runeset:     mf,
	}
}

// gatherRunesetCasts gathers all YAML casts in a directory.
func gatherRunesetCasts(castsDir string) ([]string, error) {
	castFiles, err := utils.GetYAMLFilesInDirectory(castsDir, false)
	if err != nil {
		return nil, err
	}
	if len(castFiles) == 0 {
		return nil, fmt.Errorf("no YAML casts found under %s", castsDir)
	}
	// Deterministic order
	slices.Sort(castFiles)
	return castFiles, nil
}

// renderRunesetCasts renders all casts in a directory into bytes.
func renderRunesetCasts(root string, castFiles []string, mergedValues map[string]interface{}, ctx map[string]interface{}) ([][]byte, error) {

	// Create renderer
	r := utils.NewRenderer(root, mergedValues, ctx)

	// Render each cast into bytes
	rendered := make([][]byte, 0, len(castFiles))
	for _, p := range castFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("failed to read cast %s: %w", p, err)
		}
		out, err := r.RenderString(string(b), 0)
		if err != nil {
			return nil, fmt.Errorf("render error in %s: %w", p, err)
		}
		rendered = append(rendered, []byte(out))
	}

	return rendered, nil

}

// extractRunesetArchive extracts a .runeset.tgz into a temporary directory and returns the path.
func extractRunesetArchive(archivePath string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	tmpDir, err := os.MkdirTemp("", "runeset-*")
	if err != nil {
		return "", err
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			return "", err
		}
		// Safely construct target path to prevent directory traversal
		target, err := utils.SafePath(tmpDir, hdr.Name)
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			return "", fmt.Errorf("unsafe archive entry: %w", err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				_ = os.RemoveAll(tmpDir)
				return "", err
			}
		case tar.TypeReg:
			// Check file size to prevent decompression bombs
			if hdr.Size > utils.MAX_FILE_SIZE {
				_ = os.RemoveAll(tmpDir)
				return "", fmt.Errorf("file too large: %s (%d bytes, max %d bytes)", hdr.Name, hdr.Size, utils.MAX_FILE_SIZE)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				_ = os.RemoveAll(tmpDir)
				return "", err
			}
			out, err := os.Create(target)
			if err != nil {
				_ = os.RemoveAll(tmpDir)
				return "", err
			}
			// Use io.CopyN with file size limit to prevent decompression bombs
			if _, err := io.CopyN(out, tr, hdr.Size); err != nil {
				out.Close()
				_ = os.RemoveAll(tmpDir)
				return "", err
			}
			out.Close()
		}
	}
	return tmpDir, nil
}

func downloadRunesetArchive(url string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "rune-cli/1 runeset-fetch")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("download failed: %s (rate limited or forbidden)", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}
	tmpFile, err := os.CreateTemp("", "runeset-*.tgz")
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()
	// Use io.CopyN with size limit to prevent decompression bombs
	if _, err := io.CopyN(tmpFile, resp.Body, utils.MAX_FILE_SIZE); err != nil {
		return "", err
	}
	return tmpFile.Name(), nil
}

// resolveGitHubRuneset supports github.com/org/repo/path@ref or full URLs returning archives.
// It downloads the tarball for the given ref and extracts the specified subdirectory.
func resolveGitHubRuneset(ref string) (string, error) {
	// Expected form: github.com/org/repo/path/to/runeset@ref
	parts := strings.Split(ref, "@")
	repoPath := parts[0]
	refName := "main"
	if len(parts) > 1 && parts[1] != "" {
		refName = parts[1]
	}
	segments := strings.Split(repoPath, "/")
	if len(segments) < 3 {
		return "", fmt.Errorf("invalid github ref: %s (expected github.com/org/repo/path@ref)", ref)
	}
	org := segments[1]
	repo := segments[2]
	subdir := ""
	if len(segments) > 3 {
		subdir = strings.Join(segments[3:], "/")
	}

	archiveURL := fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/%s", org, repo, refName)
	tmp, err := downloadRunesetArchive(archiveURL)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)

	root, err := extractRunesetArchive(tmp)
	if err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(root)
	if len(entries) == 1 && entries[0].IsDir() {
		root = filepath.Join(root, entries[0].Name())
	}
	if subdir != "" {
		root = filepath.Join(root, subdir)
	}
	if !utils.FileExists(filepath.Join(root, "runeset.yaml")) {
		_ = os.RemoveAll(root)
		return "", fmt.Errorf("runeset.yaml not found under %s (check path/ref)", root)
	}
	return root, nil
}

// getRunesetSourceType checks args for runeset sources (dir, archive, https, GitHub shorthand)
// and returns the source type.
func getRunesetSourceType(args []string) types.RunesetSourceType {
	if len(args) == 0 && utils.FileExists("runeset.yaml") {
		return types.RunesetSourceTypeDirectory
	}

	if len(args) > 1 {
		return types.RunesetSourceTypeUnknown
	}

	arg := args[0]
	// Remote URL (.runeset.tgz)
	if strings.HasPrefix(arg, "https://") {
		if strings.HasSuffix(strings.ToLower(arg), ".runeset.tgz") {
			return types.RunesetSourceTypeRemoteArchive
		}
		if strings.Contains(arg, "github.com/") {
			return types.RunesetSourceTypeGitHub
		}
	}
	// Directory runeset
	if utils.IsDirectory(arg) && utils.FileExists(filepath.Join(arg, "runeset.yaml")) {
		return types.RunesetSourceTypeDirectory
	}
	// Package runeset (.runeset.tgz)
	if utils.FileExists(arg) && strings.HasSuffix(strings.ToLower(arg), ".runeset.tgz") {
		return types.RunesetSourceTypePackageArchive
	}
	// GitHub-style shorthand: github.com/org/repo/path@ref
	if strings.HasPrefix(arg, "github.com/") {
		return types.RunesetSourceTypeGitHub
	}
	return types.RunesetSourceTypeUnknown
}

// handleRunesetCastSource handles a runeset source and runs the runeset cast.
func handleRunesetCastSource(args []string, sourceType types.RunesetSourceType, opts *castOptions) error {
	arg := args[0]
	if len(args) == 0 && sourceType == types.RunesetSourceTypeDirectory {
		arg = "."
	}
	fmt.Printf("- Runeset source: %s (%s)\n", sourceType, arg)
	// switch remains
	switch sourceType {
	case types.RunesetSourceTypeRemoteArchive:
		return handleRunesetCastSourceRemoteArchive(arg, opts)
	case types.RunesetSourceTypeGitHub:
		return handleRunesetCastSourceGitHub(arg, opts)
	case types.RunesetSourceTypePackageArchive:
		return handleRunesetCastSourcePackageArchive(arg, opts)
	case types.RunesetSourceTypeDirectory:
		return handleRunesetCastSourceDirectory(arg, opts)
	}
	return fmt.Errorf("unknown runeset source: %s", sourceType)
}

// handleRunesetCastSourceRemoteArchive downloads a remote archive and runs the runeset cast.
// It downloads the tarball and extracts the specified subdirectory.
// It is used for remote archives (e.g., https://example.com/runeset.tgz) and package archives
// (e.g., ./runeset.tgz).
func handleRunesetCastSourceRemoteArchive(source string, opts *castOptions) error {
	tmpFile, err := downloadRunesetArchive(source)
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile)
	tmpDir, err := extractRunesetArchive(tmpFile)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	return runRunesetCast(tmpDir, opts)
}

// handleRunesetCastSourceGitHub resolves a GitHub shorthand
// (e.g., github.com/org/repo/path@ref) and runs the runeset cast.
// It downloads the tarball for the given ref and extracts the specified subdirectory.
func handleRunesetCastSourceGitHub(source string, opts *castOptions) error {
	root, err := resolveGitHubRuneset(source)
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	return runRunesetCast(root, opts)
}

// handleRunesetCastSourcePackageArchive extracts a package archive and runs the runeset cast.
// It is used for package archives (e.g., ./runeset.tgz).
func handleRunesetCastSourcePackageArchive(source string, opts *castOptions) error {
	tmpDir, err := extractRunesetArchive(source)
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	return runRunesetCast(tmpDir, opts)
}

// handleRunesetCastSourceDirectory runs the runeset cast from a directory.
// It is used for directory with a runeset.yaml file (e.g., ./runeset).
func handleRunesetCastSourceDirectory(source string, opts *castOptions) error {
	return runRunesetCast(source, opts)
}
