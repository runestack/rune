package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

// Cast command flags
type castOptions struct {
	cmdOptions

	tag             string
	dryRun          bool
	detach          bool
	timeoutStr      string
	recursiveDir    bool
	forceGeneration bool
	createNamespace bool

	// Runeset / release flags
	valuesFiles []string
	setValues   []string
	renderOnly  bool
	releaseName string

	// Stateful-release flags (CAST_REFACTOR_PLAN §7)
	adopt      bool   // take over unmanaged / foreign-owned resources (--adopt)
	atomic     bool   // roll back this revision's changes on failure (--atomic, D3)
	yes        bool   // skip the confirm prompt for prune/adopt plans (--yes)
	outputJSON bool   // emit structured plan + result (--output json)
	output     string // raw --output value
}

// ResourceInfo holds information about resources to be deployed
type ResourceInfo struct {
	FilesByType          map[string][]string
	ServicesByFile       map[string][]*types.Service
	SecretsByFile        map[string][]*types.Secret
	ConfigmapsByFile     map[string][]*types.Configmap
	StorageClassesByFile map[string][]*types.StorageClass
	VolumesByFile        map[string][]*types.Volume
	TotalResources       int
	SourceArguments      []string
}

// createCmd is the umbrella command for quick create
func newCastCmd() *cobra.Command {
	opts := &castOptions{}
	cmd := &cobra.Command{
		Use:     "cast [files, directories or runeset...]",
		Short:   "Apply resources (services, secrets, configs)",
		Aliases: []string{"apply"},
		Long: `Deploy a service defined in a YAML file.
	For example:
	  rune cast my-service.yml
	  rune cast my-service.yml --namespace=production
	  rune cast my-service.yml --tag=stable
	  rune cast my-directory/ --recursive
	  rune cast services/*.yaml
	  rune cast my-service.yml --force
	  rune cast github.com/org/repo/path@ref --create-namespace
	  rune cast https://example.com/runeset.tgz --release=my-release
	  rune cast ./runeset.tgz --release=my-release
	  rune cast ./runeset --render --set=key=value
	  rune cast ./runeset --render --values=values.yaml`,
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve the release's home namespace: --namespace flag, else the
			// current context's defaultNamespace (from config), else "default".
			// A release record requires a namespace, so this must never be empty.
			opts.namespace = effectiveCmdNS(opts.namespace)
			if opts.namespace == "" {
				opts.namespace = "default"
			}
			return runCast(cmd.Context(), args, opts)
		},
	}

	// Local flags for the cast command
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "", "Namespace to deploy the service in (overrides existing namespace if specified)")
	cmd.Flags().StringVar(&opts.tag, "tag", "", "Tag for this deployment (e.g., 'stable', 'canary')")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Validate the service definition without deploying it")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "Detach from the deployment and return immediately")
	// Must exceed the update stall deadline (600s): this bounds the whole
	// Cast RPC, including the server's rollout verify, so a shorter value
	// silently overrides it — and with --atomic it reverts a healthy,
	// still-progressing deploy (RUNE-042 §8.3).
	cmd.Flags().StringVar(&opts.timeoutStr, "timeout", "15m", "Timeout for the wait operation")
	cmd.Flags().BoolVarP(&opts.recursiveDir, "recursive", "r", false, "Recursively process directories")
	cmd.Flags().BoolVar(&opts.forceGeneration, "force", false, "Force generation increment even if no changes are detected")
	cmd.Flags().BoolVar(&opts.createNamespace, "create-namespace", false, "Create the namespace if it doesn't exist")

	// Runeset / release flags
	cmd.Flags().StringArrayVarP(&opts.valuesFiles, "values", "f", []string{}, "Values file(s) to merge (repeatable; last wins)")
	cmd.Flags().StringArrayVar(&opts.setValues, "set", []string{}, "Set values on the command line (key=value; repeatable)")
	cmd.Flags().BoolVar(&opts.renderOnly, "render", false, "Render casts and print to stdout without applying")
	cmd.Flags().StringVar(&opts.releaseName, "release", "", "Release name (overrides the runeset manifest name / derived basename)")

	// Stateful-release flags (CAST_REFACTOR_PLAN §7)
	cmd.Flags().BoolVar(&opts.adopt, "adopt", false, "Take over resources that are unmanaged or owned by another release")
	cmd.Flags().BoolVar(&opts.atomic, "atomic", false, "Roll back this cast's changes if it fails (note: a verify timeout also triggers rollback)")
	cmd.Flags().BoolVar(&opts.yes, "yes", false, "Skip the confirmation prompt for plans that prune or adopt")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "", "Output format: json (emit structured plan + result)")
	return cmd
}

func init() { rootCmd.AddCommand(newCastCmd()) }

// runCast is the unified cast pipeline (CAST_REFACTOR_PLAN §3): resolve source →
// render → [render-only] → plan → [dry-run] → confirm → Cast RPC. Every cast
// produces a tracked Release; the server owns the 3-way reconcile (C1).
func runCast(ctx context.Context, args []string, opts *castOptions) error {
	startTime := time.Now()

	// Validate --output up front.
	switch strings.ToLower(opts.output) {
	case "", "json":
		opts.outputJSON = strings.EqualFold(opts.output, "json")
	default:
		return fmt.Errorf("unsupported --output %q (supported: json)", opts.output)
	}

	if _, err := time.ParseDuration(opts.timeoutStr); err != nil {
		return fmt.Errorf("invalid timeout value: %w", err)
	}

	// 1) Resolve the source to a single (ReleaseSource, releaseName, renderPlan).
	rc, err := resolveCastSource(args, opts)
	if err != nil {
		return err
	}
	if rc.cleanup != nil {
		defer rc.cleanup()
	}

	// Warn when the release name was auto-derived (C2: derive + warn).
	if rc.nameDerived && !opts.outputJSON {
		fmt.Fprintf(os.Stderr, "%s release name derived as %q from the source; pin it with --release to make it permanent\n",
			format.Warning("warning:"), rc.releaseName)
	}

	// --render: print rendered casts and stop (no server contact, no resource
	// extraction). Short-circuits before the full render so it stays cheap.
	if opts.renderOnly {
		return printRenderedCasts(rc, opts)
	}

	// 2) Render → desired resource set + payloads (client renders; C1).
	rendered, err := renderResolvedCast(rc, opts)
	if err != nil {
		return err
	}

	if rendered.totalResources() == 0 {
		return fmt.Errorf("no resources found to deploy in %s", rc.source.Location)
	}

	// Lint the fully-rendered resources (names are final here) so malformed
	// manifests fail fast with a clear error, before planning or applying.
	if err := rendered.lint(); err != nil {
		return err
	}

	spec := rendered.toReleaseSpec(rc.releaseName, rc.source, opts)

	// 3) Connect and compute the plan (online — needed by dry-run, confirm,
	// detach pre-check, and JSON output; C4/C5).
	apiClient, err := newAPIClient("", "")
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()
	rcl := client.NewReleaseClient(apiClient)

	// Resolve cast-time `{{ secret:... }}` templates in secret payloads before
	// shipping them (RUNE-105). Out-of-release components are revealed via the API.
	if err := rendered.resolveSecretTemplates(apiClient); err != nil {
		return err
	}

	plan, err := rcl.PlanSpec(spec)
	if err != nil {
		return err
	}

	// 4) --dry-run: render the plan block and stop (applies nothing — C4).
	if opts.dryRun {
		if opts.outputJSON {
			return writeCastJSON(buildCastJSON(rc.releaseName, rendered.namespace, true, plan, nil))
		}
		printCastBanner([]string{rc.source.Location}, opts.detach)
		renderPlanBlock(os.Stdout, rc.releaseName, rendered.namespace, 0, plan)
		fmt.Println()
		printUpdateWarnings(os.Stdout, rendered.updateWarnings())
		fmt.Println(format.Dim("dry-run: nothing was applied. Re-run without --dry-run to apply."))
		return nil
	}

	// 5) Detach pre-check (C3): refuse --detach when the plan prunes. We surface
	// it here (before any mutation) rather than letting the server reject mid-flight.
	if opts.detach && planHasPrune(plan) {
		return fmt.Errorf("--detach is not allowed: this plan prunes resources (detach is create/update-only)\n"+
			"hint: drop --detach to apply the destructive plan, or run 'rune release diff %s' to inspect it", rc.releaseName)
	}

	// 6) Display the plan, then confirm before applying anything destructive (C5).
	if !opts.outputJSON {
		printCastBanner([]string{rc.source.Location}, opts.detach)
		renderPlanBlock(os.Stdout, rc.releaseName, rendered.namespace, 0, plan)
		fmt.Println()
		// Under the plan and above the confirm prompt: the last place an
		// operator is still deciding.
		printUpdateWarnings(os.Stdout, rendered.updateWarnings())
	}
	if !plan.Applyable {
		return fmt.Errorf("plan has unresolved ownership conflicts; pass --adopt to take ownership")
	}
	ok, err := confirmApply(plan, opts)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "aborted.")
		return fmt.Errorf("cast aborted by user")
	}

	// 7) Apply via the Cast RPC (server reconciles: create → update → verify →
	// prune-last; C1). A spinner shows activity while the blocking reconcile runs
	// (rune-scale-style loading UX); detach returns near-instantly.
	timeout, _ := time.ParseDuration(opts.timeoutStr) // validated above
	payloads := client.CastPayloads{
		Services:   rendered.payloads.services,
		Secrets:    rendered.payloads.secrets,
		Configmaps: rendered.payloads.configmaps,
		Volumes:    rendered.payloads.volumes,
	}
	applyLabel := "Applying…"
	if opts.detach {
		applyLabel = "Submitting…"
	}
	var rel *types.Release
	var appliedPlan *client.Plan
	apply := func() error {
		var e error
		rel, appliedPlan, e = rcl.Cast(spec, payloads, timeout)
		return e
	}
	if opts.outputJSON {
		err = apply()
	} else {
		err = runWithSpinner(applyLabel, apply)
	}
	if err != nil {
		if errors.Is(err, client.ErrDetachWouldPrune) {
			return fmt.Errorf("--detach is not allowed: this plan prunes resources (detach is create/update-only)")
		}
		return err
	}
	if appliedPlan != nil {
		plan = appliedPlan
	}

	// 8) Emit result.
	if opts.outputJSON {
		return writeCastJSON(buildCastJSON(rc.releaseName, rendered.namespace, false, plan, releaseToResult(rel)))
	}
	printCastReleaseSummary(rel, startTime, opts)
	return nil
}

// releaseToResult projects an applied Release for the JSON output.
func releaseToResult(rel *types.Release) *castReleaseResult {
	if rel == nil {
		return nil
	}
	out := &castReleaseResult{Revision: rel.Revision, Status: string(rel.Status)}
	for _, o := range rel.Owns {
		out.Owns = append(out.Owns, castJSONResource{
			ResourceType: string(o.ResourceType),
			Namespace:    o.Namespace,
			Name:         o.Name,
		})
	}
	return out
}

// printRenderedCasts prints the rendered castfile set to stdout (the --render
// path), re-rendering bytes so the output mirrors what the server would apply.
func printRenderedCasts(rc *resolvedCast, opts *castOptions) error {
	blocks, err := renderCastBytes(rc, opts)
	if err != nil {
		return err
	}
	for _, b := range blocks {
		fmt.Println(string(b))
		fmt.Println("---")
	}
	return nil
}

// printCastReleaseSummary prints the post-apply summary (C5 step 4): revision,
// owned resources, timing, and the follow-up command hints.
func printCastReleaseSummary(rel *types.Release, startTime time.Time, opts *castOptions) {
	if rel == nil {
		return
	}
	fmt.Println()
	if opts.detach {
		fmt.Printf("%s Release %q accepted (revision %d) — completing in background.\n",
			format.Success("✓"), rel.Name, rel.Revision)
	} else {
		fmt.Printf("%s Release %q deployed (revision %d, status %s)\n",
			format.Success("✓"), rel.Name, rel.Revision, rel.Status)
	}
	if len(rel.Owns) > 0 {
		fmt.Println("\nOwned resources:")
		for _, o := range rel.Owns {
			fmt.Printf("  - %s %s/%s\n", o.ResourceType, o.Namespace, o.Name)
		}
	}
	fmt.Println("\nNext:")
	fmt.Printf("  rune release status %s -n %s\n", rel.Name, rel.Namespace)
	fmt.Printf("  rune release history %s -n %s\n", rel.Name, rel.Namespace)
	fmt.Printf("\n%s\n", format.Dim("done in %.1fs", time.Since(startTime).Seconds()))
}

// processResourceFiles loads, categorizes, and validates resources from files
// parseCastFilesResources reads and validates cast files, returning discovered resources.
// It collects errors across files and reports them in a consolidated form.
//
// values, when non-empty, runs each cast file's bytes through the template
// engine before parsing — supports `--values` and `--set` on plain cast
// files in the same shape the runeset path uses. Pass nil for the
// zero-templating fast path.
func parseCastFilesResources(filePaths []string, sourceArgs []string, opts *castOptions, values map[string]interface{}) (*ResourceInfo, error) {
	info := &ResourceInfo{
		FilesByType:          make(map[string][]string),
		ServicesByFile:       make(map[string][]*types.Service),
		SecretsByFile:        make(map[string][]*types.Secret),
		ConfigmapsByFile:     make(map[string][]*types.Configmap),
		StorageClassesByFile: make(map[string][]*types.StorageClass),
		VolumesByFile:        make(map[string][]*types.Volume),
		TotalResources:       0,
		SourceArguments:      sourceArgs,
	}

	// Print detected resources header
	fmt.Println("🧩 Validating specifications...")

	var errorMessages []string

	// Iterate files: parse, lint, convert
	for _, filePath := range filePaths {
		fileName := filepath.Base(filePath)

		// Format the filename with fixed width padding for display
		fileNameDisplay := fmt.Sprintf("- %-20s", fileName)

		// Print filename (without newline)
		fmt.Print(fileNameDisplay)

		// Calculate and print dots
		totalWidth := 35
		dotsNeeded := totalWidth - len(fileNameDisplay)
		if dotsNeeded < 3 {
			dotsNeeded = 3
		}
		fmt.Print(strings.Repeat(".", dotsNeeded))
		fmt.Print(" ") // Space before validation result

		// Parse as CastFile to extract all resources. When --values /
		// --set were supplied, run the file through the template
		// engine first so `{{ values.foo }}` references resolve in
		// the same shape they would inside a runeset.
		raw, readErr := os.ReadFile(filePath)
		if readErr != nil {
			fmt.Println("❌")
			return nil, fmt.Errorf("failed to read file %s: %w", filePath, readErr)
		}
		rendered, renderErr := renderCastFileBytes(filePath, raw, values, opts.namespace, castMode(opts))
		if renderErr != nil {
			fmt.Println("❌")
			return nil, renderErr
		}
		castFile, err := types.ParseCastFileFromBytes(rendered, opts.namespace)
		if err != nil {
			fmt.Println("❌") // Show failure
			return nil, fmt.Errorf("failed to parse file %s: %w", filePath, err)
		}

		// Per-file error accumulator
		var fileErrors []string

		// Lint all specs in the file first - fail fast on validation errors
		if lintErrs := castFile.Lint(); len(lintErrs) > 0 {
			fmt.Println("❌")
			for _, le := range lintErrs {
				fileErrors = append(fileErrors, le.Error())
			}
			// Don't continue processing - validation failed
			// Stash file errors with filename context and continue to next file
			for _, fe := range fileErrors {
				errorMessages = append(errorMessages, fmt.Sprintf("%s: %s", fileName, fe))
			}
			continue
		}

		// Show success checkmark if all validations passed
		fmt.Println("✓")

		// Extract services from the cast file with proper error handling
		services, err := castFile.GetServices()
		if err != nil {
			fileErrors = append(fileErrors, fmt.Sprintf("failed to extract services: %v", err))
		} else if len(services) > 0 {
			info.ServicesByFile[filePath] = services
			info.TotalResources += len(services)

			// Fix FilesByType logic: initialize empty slice first, then append if not present
			if _, exists := info.FilesByType["Service"]; !exists {
				info.FilesByType["Service"] = []string{}
			}
			if !stringSliceContains(info.FilesByType["Service"], fileName) {
				info.FilesByType["Service"] = append(info.FilesByType["Service"], fileName)
			}
		}

		// Extract secrets from the cast file with proper error handling
		secrets, err := castFile.GetSecrets()
		if err != nil {
			fileErrors = append(fileErrors, fmt.Sprintf("failed to extract secrets: %v", err))
		} else if len(secrets) > 0 {
			info.SecretsByFile[filePath] = secrets
			info.TotalResources += len(secrets)

			// Fix FilesByType logic
			if _, exists := info.FilesByType["Secret"]; !exists {
				info.FilesByType["Secret"] = []string{}
			}
			if !stringSliceContains(info.FilesByType["Secret"], fileName) {
				info.FilesByType["Secret"] = append(info.FilesByType["Secret"], fileName)
			}
		}

		// Extract config maps from the cast file with proper error handling
		configmaps, err := castFile.GetConfigmaps()
		if err != nil {
			fileErrors = append(fileErrors, fmt.Sprintf("failed to extract config maps: %v", err))
		} else if len(configmaps) > 0 {
			info.ConfigmapsByFile[filePath] = configmaps
			info.TotalResources += len(configmaps)

			// Fix FilesByType logic
			if _, exists := info.FilesByType["Configmap"]; !exists {
				info.FilesByType["Configmap"] = []string{}
			}
			if !stringSliceContains(info.FilesByType["Configmap"], fileName) {
				info.FilesByType["Configmap"] = append(info.FilesByType["Configmap"], fileName)
			}
		}

		// Extract storage classes from the cast file (RUNE-072)
		storageClasses, err := castFile.GetStorageClasses()
		if err != nil {
			fileErrors = append(fileErrors, fmt.Sprintf("failed to extract storage classes: %v", err))
		} else if len(storageClasses) > 0 {
			info.StorageClassesByFile[filePath] = storageClasses
			info.TotalResources += len(storageClasses)
			if _, exists := info.FilesByType["StorageClass"]; !exists {
				info.FilesByType["StorageClass"] = []string{}
			}
			if !stringSliceContains(info.FilesByType["StorageClass"], fileName) {
				info.FilesByType["StorageClass"] = append(info.FilesByType["StorageClass"], fileName)
			}
		}

		// Extract volumes from the cast file (RUNE-072)
		volumes, err := castFile.GetVolumes()
		if err != nil {
			fileErrors = append(fileErrors, fmt.Sprintf("failed to extract volumes: %v", err))
		} else if len(volumes) > 0 {
			info.VolumesByFile[filePath] = volumes
			info.TotalResources += len(volumes)
			if _, exists := info.FilesByType["Volume"]; !exists {
				info.FilesByType["Volume"] = []string{}
			}
			if !stringSliceContains(info.FilesByType["Volume"], fileName) {
				info.FilesByType["Volume"] = append(info.FilesByType["Volume"], fileName)
			}
		}

		// If we have extraction errors, collect them
		if len(fileErrors) > 0 {
			for _, fe := range fileErrors {
				errorMessages = append(errorMessages, fmt.Sprintf("%s: %s", fileName, fe))
			}
		}
	}

	fmt.Println()

	if len(errorMessages) > 0 {
		fmt.Println("❌ Validation errors:")
		for _, m := range errorMessages {
			fmt.Printf("  - %s\n", m)
		}
		return nil, fmt.Errorf("validation failed for one or more files")
	}

	// Print detected resources
	printResourceInfo(info, opts)

	return info, nil
}

// printCastBanner prints the initial banner for the cast command
func printCastBanner(args []string, isDetached bool) {
	if isDetached {
		fmt.Println("\n🔮 Rune Cast Initiated (Detached Mode)")
	} else {
		fmt.Println("\n🔮 Rune Cast Initiated")
	}

	// Print source info
	fmt.Println("\n- Source:", format.Highlight("%s", strings.Join(args, ", ")))
	fmt.Println()
}

// printResourceInfo displays information about detected resources
func printResourceInfo(info *ResourceInfo, opts *castOptions) {
	// Print detected resources
	fmt.Printf("- Detected %d resources:\n", info.TotalResources)
	for resourceType, files := range info.FilesByType {
		for _, file := range files {
			fmt.Printf("  - %s: %s\n", resourceType, file)
		}
	}

	if opts.namespace != "" {
		fmt.Println("- Namespace:", format.Highlight("%s", opts.namespace))
	} else {
		fmt.Println("- Namespace:", format.Highlight("from resource definition"))
	}

	targetDisplay := getTargetRuneServer()
	fmt.Printf("- Target: %s (%s)\n", format.Highlight("%s", targetDisplay), targetDisplay)
	fmt.Println()

	if opts.dryRun {
		fmt.Println("🧪 Running in dry-run mode (validation only)")
	}
}

// contains checks if a string slice contains a specific value
func stringSliceContains(slice []string, value string) bool {
	for _, item := range slice {
		if item == value {
			return true
		}
	}
	return false
}

// printInitStepProgress emits one line per init-step transition
// observed across the service's instances since the last poll. Used by
// `rune cast` to show the user "▶ format..." then "✓ format succeeded"
// while the service sits in Initializing (RUNE-121).
//
// The seen map is mutated in place: callers create one fresh map per
// `waitForServiceReady` invocation and pass it on every tick. Output
// goes to w (stderr in production, a buffer under test).
func printInitStepProgress(w io.Writer, svc *types.Service, seen map[string]types.InitStepStatus) {
	if svc == nil {
		return
	}
	for _, inst := range svc.Instances {
		for _, st := range inst.InitStates {
			key := inst.Name + "/" + st.Name
			if seen[key] == st.Status {
				continue
			}
			seen[key] = st.Status
			switch st.Status {
			case types.InitStepStatusRunning:
				fmt.Fprintf(w, "    ▶ init step %q on %s\n", st.Name, inst.Name)
			case types.InitStepStatusSucceeded:
				fmt.Fprintf(w, "    ✓ init step %q on %s succeeded\n", st.Name, inst.Name)
			case types.InitStepStatusSkipped:
				msg := st.Message
				if msg == "" {
					msg = "skipped"
				}
				fmt.Fprintf(w, "    ↷ init step %q on %s: %s\n", st.Name, inst.Name, msg)
			case types.InitStepStatusFailed:
				reason := st.Reason
				if reason == "" {
					reason = "Failed"
				}
				fmt.Fprintf(w, "    ✗ init step %q on %s failed: %s\n", st.Name, inst.Name, reason)
				if st.Message != "" {
					fmt.Fprintf(w, "        %s\n", st.Message)
				}
			}
		}
	}
}
