package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/cli/utils"
	"github.com/runestack/rune/pkg/log"
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

	// Runeset flags
	valuesFiles []string
	setValues   []string
	renderOnly  bool
	releaseName string
}

// ResourceInfo holds information about resources to be deployed
type ResourceInfo struct {
	FilesByType      map[string][]string
	ServicesByFile   map[string][]*types.Service
	SecretsByFile    map[string][]*types.Secret
	ConfigmapsByFile map[string][]*types.Configmap
	TotalResources   int
	SourceArguments  []string
}

// DeploymentResult holds results of a deployment
type DeploymentResult struct {
	SuccessfulResources map[string][]string
	FailedResources     map[string]string
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
			opts.namespace = effectiveCmdNS(opts.namespace)
			return runCast(cmd.Context(), args, opts)
		},
	}

	// Local flags for the cast command
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "", "Namespace to deploy the service in (overrides existing namespace if specified)")
	cmd.Flags().StringVar(&opts.tag, "tag", "", "Tag for this deployment (e.g., 'stable', 'canary')")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Validate the service definition without deploying it")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "Detach from the deployment and return immediately")
	cmd.Flags().StringVar(&opts.timeoutStr, "timeout", "5m", "Timeout for the wait operation")
	cmd.Flags().BoolVarP(&opts.recursiveDir, "recursive", "r", false, "Recursively process directories")
	cmd.Flags().BoolVar(&opts.forceGeneration, "force", false, "Force generation increment even if no changes are detected")
	cmd.Flags().BoolVar(&opts.createNamespace, "create-namespace", false, "Create the namespace if it doesn't exist")

	// Runeset-related flags
	cmd.Flags().StringArrayVarP(&opts.valuesFiles, "values", "f", []string{}, "Values file(s) to merge (repeatable; last wins)")
	cmd.Flags().StringArrayVar(&opts.setValues, "set", []string{}, "Set values on the command line (key=value; repeatable)")
	cmd.Flags().BoolVar(&opts.renderOnly, "render", false, "Render runeset casts and print to stdout without applying")
	cmd.Flags().StringVar(&opts.releaseName, "release", "", "Override name used in runeset manifest")
	return cmd
}

func init() { rootCmd.AddCommand(newCastCmd()) }

func runCast(ctx context.Context, args []string, opts *castOptions) error {
	// Parse timeout duration
	timeout, err := time.ParseDuration(opts.timeoutStr)
	if err != nil {
		return fmt.Errorf("invalid timeout value: %w", err)
	}

	// Delegate runeset/remote handling
	if sourceType := getRunesetSourceType(args); sourceType != types.RunesetSourceTypeUnknown {
		return handleRunesetCastSource(args, sourceType, opts)
	}

	// Warn if runeset-only flags are set in non-runeset mode
	if opts.renderOnly || len(opts.valuesFiles) > 0 || len(opts.setValues) > 0 || opts.releaseName != "" {
		fmt.Println("Note: runeset-specific flags (--render, --values, --set, --release) are ignored in non-runeset mode")
	}

	return runCastNonRuneset(args, timeout, opts)
}

func runCastNonRuneset(args []string, timeout time.Duration, opts *castOptions) error {
	startTime := time.Now()

	// Fallback to existing behaviors (single files or arbitrary folders)
	// Expand file paths (including directories and glob patterns)
	filePaths, err := utils.ExpandFilePaths(args, opts.recursiveDir)
	if err != nil {
		return err
	}

	if len(filePaths) == 0 {
		return fmt.Errorf("no service files found")
	}

	// Print initial cast banner
	printCastBanner(args, opts.detach)

	// Load, categorize, and validate resources using CastFile
	resourceInfo, err := parseCastFilesResources(filePaths, args, opts)
	if err != nil {
		return err
	}

	// If dry-run is enabled, we're done here
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

	// Print deployment preparation
	fmt.Println("🚀 Preparing deployment plan...")
	fmt.Println()

	// Deploy resources
	deploymentResults, err := deployResources(apiClient, resourceInfo, timeout, opts)
	if err != nil {
		return err
	}

	// Print summary based on deployment mode
	if opts.detach {
		printDetachedModeSummary(deploymentResults, startTime)
	} else {
		printWatchModeSummary(deploymentResults, startTime, opts)
	}

	// Capacity and allocation summary (single-node, best-effort)
	if verbose {
		// Pass namespace for capacity summary (empty string means all namespaces)
		if err := printCapacityAndAllocationSummary(apiClient, opts.namespace); err != nil {
			// Non-fatal; log and continue
			fmt.Println("Note: capacity/allocation summary unavailable:", err.Error())
		}
	}

	return nil
}

// processResourceFiles loads, categorizes, and validates resources from files
// parseCastFilesResources reads and validates cast files, returning discovered resources.
// It collects errors across files and reports them in a consolidated form.
func parseCastFilesResources(filePaths []string, sourceArgs []string, opts *castOptions) (*ResourceInfo, error) {
	info := &ResourceInfo{
		FilesByType:      make(map[string][]string),
		ServicesByFile:   make(map[string][]*types.Service),
		SecretsByFile:    make(map[string][]*types.Secret),
		ConfigmapsByFile: make(map[string][]*types.Configmap),
		TotalResources:   0,
		SourceArguments:  sourceArgs,
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

		// Parse as CastFile to extract all resources
		castFile, err := types.ParseCastFile(filePath, opts.namespace)
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

func deployResources(apiClient *client.Client, info *ResourceInfo, timeout time.Duration, opts *castOptions) (*DeploymentResult, error) {
	results := &DeploymentResult{
		SuccessfulResources: make(map[string][]string),
		FailedResources:     make(map[string]string),
	}

	// Initialize resource type maps
	results.SuccessfulResources["Service"] = []string{}
	results.SuccessfulResources["Secret"] = []string{}
	results.SuccessfulResources["Configmap"] = []string{}

	// Safety check: ensure we have resources to deploy
	if info.TotalResources == 0 {
		return results, fmt.Errorf("no resources found to deploy")
	}

	// Deploy Configmaps and Secrets first, then Services
	if err := deployConfigmaps(apiClient, info, results, opts); err != nil {
		return results, err
	}
	if err := deploySecrets(apiClient, info, results, opts); err != nil {
		return results, err
	}
	if err := deployServices(apiClient, info, results, timeout, opts); err != nil {
		return results, err
	}

	return results, nil
}

func deployServices(apiClient *client.Client, info *ResourceInfo, results *DeploymentResult, timeout time.Duration, opts *castOptions) error {
	serviceClient := client.NewServiceClient(apiClient)
	// Calculate actual resource count safely
	resourceCount := 0
	for _, services := range info.ServicesByFile {
		resourceCount += len(services)
	}

	// Safety check: no services to deploy
	if resourceCount == 0 {
		return nil
	}

	resourceIndex := 0

	for _, services := range info.ServicesByFile {
		for _, service := range services {
			resourceIndex++

			// Determine resource type for display
			resourceType := "Service"

			// Check if service already exists - determine if creating or updating
			action := "Creating"
			_, err := serviceClient.GetService(service.Namespace, service.Name)
			if err == nil {
				action = "Deploying"
			}

			fmt.Printf("  [%d/%d] %s Service \"%s\" ", resourceIndex, resourceCount, action, format.Highlight(service.Name))
			fmt.Print(strings.Repeat(".", 25-len(service.Name)))

			// Deploy the service; server handles update-on-exists
			if err := serviceClient.CreateService(service, opts.createNamespace); err != nil {
				fmt.Println(" ❌")
				results.FailedResources[service.Name] = err.Error()
				return fmt.Errorf("failed to apply service %s: %w", service.Name, err)
			}
			fmt.Println(" ✓")

			// Add to successful resources
			results.SuccessfulResources[resourceType] = append(results.SuccessfulResources[resourceType], service.Name)

			// If detach is not enabled, wait for the service to be ready
			if !opts.detach {
				fmt.Printf("    Waiting for service to be ready...")
				if err := waitForServiceReady(serviceClient, service.Namespace, service.Name, timeout); err != nil {
					fmt.Println(" ❌")
					results.FailedResources[service.Name] = err.Error()
					return fmt.Errorf("service %s failed to become ready: %w", service.Name, err)
				}
				fmt.Println(" ✓")
			}
		}
	}
	return nil
}

func deploySecrets(apiClient *client.Client, info *ResourceInfo, results *DeploymentResult, opts *castOptions) error {
	secretClient := client.NewSecretClient(apiClient)
	// Calculate actual resource count safely
	resourceCount := 0
	for _, secrets := range info.SecretsByFile {
		resourceCount += len(secrets)
	}

	// Safety check: no secrets to deploy
	if resourceCount == 0 {
		return nil
	}

	resourceIndex := 0

	for _, secrets := range info.SecretsByFile {
		for _, secret := range secrets {
			resourceIndex++

			fmt.Printf("  [%d/%d] Creating Secret \"%s\" ", resourceIndex, resourceCount, format.Highlight(secret.Name))
			fmt.Print(strings.Repeat(".", 25-len(secret.Name)))

			if err := secretClient.CreateSecret(secret, opts.createNamespace); err != nil {
				if uerr := secretClient.UpdateSecret(secret, true); uerr != nil {
					fmt.Println(" ❌")
					results.FailedResources[secret.Name] = uerr.Error()
					return fmt.Errorf("failed to apply secret %s: %w", secret.Name, uerr)
				}
			}
			fmt.Println(" ✓")
			results.SuccessfulResources["Secret"] = append(results.SuccessfulResources["Secret"], secret.Name)
		}
	}
	return nil
}

func deployConfigmaps(apiClient *client.Client, info *ResourceInfo, results *DeploymentResult, opts *castOptions) error {
	configClient := client.NewConfigmapClient(apiClient)
	// Calculate actual resource count safely
	resourceCount := 0
	for _, configmaps := range info.ConfigmapsByFile {
		resourceCount += len(configmaps)
	}

	// Safety check: no config maps to deploy
	if resourceCount == 0 {
		return nil
	}

	resourceIndex := 0

	for _, configmaps := range info.ConfigmapsByFile {
		for _, configmap := range configmaps {
			resourceIndex++

			fmt.Printf("  [%d/%d] Creating Config \"%s\" ", resourceIndex, resourceCount, format.Highlight(configmap.Name))
			fmt.Print(strings.Repeat(".", 25-len(configmap.Name)))
			if err := configClient.CreateConfigmap(configmap, opts.createNamespace); err != nil {
				if uerr := configClient.UpdateConfigmap(configmap); uerr != nil {
					fmt.Println(" ❌")
					results.FailedResources[configmap.Name] = uerr.Error()
					return fmt.Errorf("failed to apply config %s: %w", configmap.Name, uerr)
				}
			}
			fmt.Println(" ✓")
			results.SuccessfulResources["Configmap"] = append(results.SuccessfulResources["Configmap"], configmap.Name)
		}
	}
	return nil
}

// printCastBanner prints the initial banner for the cast command
func printCastBanner(args []string, isDetached bool) {
	if isDetached {
		fmt.Println("\n🔮 Rune Cast Initiated (Detached Mode)")
	} else {
		fmt.Println("\n🔮 Rune Cast Initiated")
	}

	// Print source info
	fmt.Println("\n- Source:", format.Highlight(strings.Join(args, ", ")))
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
		fmt.Println("- Namespace:", format.Highlight(opts.namespace))
	} else {
		fmt.Println("- Namespace:", format.Highlight("from resource definition"))
	}

	targetDisplay := getTargetRuneServer()
	fmt.Printf("- Target: %s (%s)\n", format.Highlight(targetDisplay), targetDisplay)
	fmt.Println()

	if opts.dryRun {
		fmt.Println("🧪 Running in dry-run mode (validation only)")
	}
}

// printDetachedModeSummary prints a summary for detached mode
func printDetachedModeSummary(results *DeploymentResult, startTime time.Time) {
	fmt.Println("🛫 Deployment initiated (no watch mode).")
	fmt.Println()
	fmt.Println("💬 Status commands:")

	// Suggest some commands for checking status
	for resourceType, names := range results.SuccessfulResources {
		if resourceType == "Service" || resourceType == "Function" {
			for _, name := range names {
				fmt.Printf("- rune trace %s\n", name)
			}
		}
	}
	fmt.Println("- rune status")
	fmt.Println("- rune list")

	elapsedTime := time.Since(startTime).Seconds()
	fmt.Printf("\n🏁 Done in %.1fs (deployment is ongoing in background).\n", elapsedTime)
}

// printWatchModeSummary prints a summary for watch mode
func printWatchModeSummary(results *DeploymentResult, startTime time.Time, opts *castOptions) {
	fmt.Println("✅ Successfully deployed:")

	// Show resources with endpoints where applicable
	for resourceType, names := range results.SuccessfulResources {
		for _, name := range names {
			endpoint := ""
			switch resourceType {
			case "Service":
				if opts.namespace != "" {
					endpoint = fmt.Sprintf(" (endpoint: http://%s.%s.rune)", name, opts.namespace)
				} else {
					endpoint = " (endpoint: namespace from resource definition)"
				}
			case "Function":
				if opts.namespace != "" {
					endpoint = fmt.Sprintf(" (endpoint: http://%s.%s.rune)", name, opts.namespace)
				} else {
					endpoint = " (endpoint: namespace from resource definition)"
				}
			}
			fmt.Printf("- %s: %s%s\n", resourceType, name, endpoint)
		}
	}

	// Show tips
	fmt.Println()
	fmt.Println("🪄 Tips:")

	// Add service-specific tips
	for resourceType, names := range results.SuccessfulResources {
		switch resourceType {
		case "Service":
			for _, name := range names {
				fmt.Printf("- Monitor: rune trace %s\n", name)
				fmt.Printf("- Scale Service: rune scale %s --replicas=2\n", name)
			}
		case "Function":
			for _, name := range names {
				fmt.Printf("- Function Logs: rune trace %s\n", name)
			}
		}
	}

	fmt.Println("- List Resources: rune list")

	elapsedTime := time.Since(startTime).Seconds()
	fmt.Printf("\n🎉 All resources ready in %.1fs\n", elapsedTime)
}

// printCapacityAndAllocationSummary prints single-node capacity and allocated requests summary
func printCapacityAndAllocationSummary(apiClient *client.Client, namespace string) error {
	fmt.Println()
	fmt.Println("📊 Node capacity and allocation (MVP, best-effort)")
	// Fetch capacity from server health (cached at startup)
	hc := client.NewHealthClient(apiClient)
	cpuCapacity, memCapacity, err := hc.GetAPIServerCapacity()
	if err != nil {
		return err
	}

	// Sum allocated (Requests) across active instances in the namespace
	instClient := client.NewInstanceClient(apiClient)

	// Handle empty namespace - list instances from all namespaces
	var instances []*types.Instance
	if namespace != "" {
		instances, err = instClient.ListInstances(namespace, "", "", "")
	} else {
		instances, err = instClient.ListInstances("", "", "", "")
	}

	if err != nil {
		return fmt.Errorf("failed to list instances: %w", err)
	}

	var cpuAllocated float64
	var memAllocated int64
	for _, inst := range instances {
		if !isInstanceCounted(inst) {
			continue
		}
		if inst.Resources != nil {
			if inst.Resources.CPU.Request != "" {
				if v, err := types.ParseCPU(inst.Resources.CPU.Request); err == nil {
					cpuAllocated += v
				}
			}
			if inst.Resources.Memory.Request != "" {
				if v, err := types.ParseMemory(inst.Resources.Memory.Request); err == nil {
					memAllocated += v
				}
			}
		}
	}

	cpuRemaining := cpuCapacity - cpuAllocated
	if cpuRemaining < 0 {
		cpuRemaining = 0
	}
	memRemaining := memCapacity - memAllocated
	if memRemaining < 0 {
		memRemaining = 0
	}

	// Print summary
	fmt.Printf("Capacity: CPU %.1f cores, Allocated %.1f, Remaining %.1f | Mem %s, Allocated %s, Remaining %s\n",
		cpuCapacity, cpuAllocated, cpuRemaining,
		types.FormatMemory(memCapacity), types.FormatMemory(memAllocated), types.FormatMemory(memRemaining))

	// Warn on over-allocation
	if cpuAllocated > cpuCapacity {
		fmt.Printf("Warning: requested CPU %.1f cores exceeds capacity %.1f cores (deployment allowed in MVP)\n", cpuAllocated, cpuCapacity)
	}
	if memAllocated > memCapacity {
		fmt.Printf("Warning: requested memory %s exceeds capacity %s (deployment allowed in MVP)\n",
			types.FormatMemory(memAllocated), types.FormatMemory(memCapacity))
	}

	return nil
}

// isInstanceCounted returns true if the instance should count toward allocated resources
func isInstanceCounted(inst *types.Instance) bool {
	switch inst.Status {
	case types.InstanceStatusRunning, types.InstanceStatusPending, types.InstanceStatusStarting, types.InstanceStatusCreated:
		return true
	default:
		return false
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

// waitForServiceReady waits for a service to be fully deployed.
func waitForServiceReady(serviceClient *client.ServiceClient, namespace, name string, timeout time.Duration) error {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Poll interval
	pollInterval := 1 * time.Second

	// Create a ticker for polling
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Timeout
			return fmt.Errorf("timeout waiting for service %s to be ready", name)
		case <-ticker.C:
			// Poll the service status
			service, err := serviceClient.GetService(namespace, name)

			if err != nil {
				log.Debug("Error getting service status", log.Err(err))
				continue
			}

			// Check if the service is running
			if service.Status == types.ServiceStatusRunning {
				// Check that all instances are ready
				allReady := true
				readyCount := 0

				for _, instance := range service.Instances {
					if instance.Status == "Running" || instance.Status == "Ready" {
						readyCount++
					} else {
						allReady = false
					}
				}

				// If all instances are ready, or at least some are ready and the service scale is met
				if (allReady && len(service.Instances) > 0) || (readyCount >= service.Scale) {
					return nil
				}
			}
		}
	}
}
