package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

// deleteOptions holds the options for the delete command
type deleteOptions struct {
	cmdOptions

	force          bool
	timeoutSeconds int32
	detach         bool
	dryRun         bool
	gracePeriod    int32
	now            bool
	ignoreNotFound bool
	finalizers     []string
	output         string
	noDependencies bool
}

// newDeleteCmd creates the delete command
func newDeleteCmd() *cobra.Command {
	opts := &deleteOptions{}
	cmd := &cobra.Command{
		Use:     "delete",
		Aliases: []string{"remove", "rm"},
		Short:   "Delete targets and manage deletion operations",
		Long: `Delete targets and manage deletion operations.

This command provides a safe and controlled way to remove targets (services, functions, etc.) 
from the Rune platform and manage ongoing deletion operations.

Available subcommands:
  delete service <service-name>  - Delete a service and its resources
  delete list                    - List deletion operations
  delete status <deletion-id>    - Get status of a deletion operation

Shorthand usage:
  delete <target>                - Direct target deletion (currently supports services)

Examples:
  # Delete a service using shorthand
  rune delete my-service

  # Delete a service using full command
  rune delete service my-service

  # Delete a service with flags
  rune delete my-service --force --output json

  # List all deletion operations
  rune delete list

  # List deletion operations in a specific namespace
  rune delete list --namespace production

  # Get status of a specific deletion
  rune delete status abc123-def456`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.namespace = effectiveCmdNS(opts.namespace)
			return runDelete(cmd, args, opts)
		},
	}

	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace of the target")
	cmd.Flags().BoolVarP(&opts.force, "force", "f", false, "Skip confirmation prompt")
	cmd.Flags().Int32VarP(&opts.timeoutSeconds, "timeout", "t", 30, "Graceful shutdown timeout in seconds")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "Start deletion and return immediately")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Show what would be deleted without actually deleting")
	cmd.Flags().Int32Var(&opts.gracePeriod, "grace-period", 0, "Grace period for graceful shutdown (alternative to --timeout)")
	cmd.Flags().BoolVar(&opts.now, "now", false, "Immediate deletion without graceful shutdown")
	cmd.Flags().BoolVar(&opts.ignoreNotFound, "ignore-not-found", false, "Don't error if target doesn't exist")
	cmd.Flags().StringSliceVar(&opts.finalizers, "finalizers", nil, "Optional finalizers to run")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "text", "Output format (text, json, yaml)")
	cmd.Flags().BoolVar(&opts.noDependencies, "no-dependencies", false, "Ignore dependents and proceed with deletion")

	// Mark mutually exclusive flags
	cmd.MarkFlagsMutuallyExclusive("detach", "now")
	cmd.MarkFlagsMutuallyExclusive("timeout", "grace-period")

	// Add subcommands
	cmd.AddCommand(newDeleteServiceCmd())
	cmd.AddCommand(newDeleteListCmd())
	cmd.AddCommand(newDeleteStatusCmd())

	return cmd
}

func runDelete(cmd *cobra.Command, args []string, opts *deleteOptions) error {
	// If no subcommand is specified and a target is provided, treat it as shorthand
	if len(args) > 0 {
		target := args[0]
		// Try deleting as a service first
		if err := runServiceDelete(cmd.Context(), target, opts); err != nil {
			// Only fall back to secret/config when the service truly doesn't exist
			if strings.Contains(strings.ToLower(err.Error()), "not found") {
				// Secret path
				if err := runDeleteSecret(cmd.Context(), target, opts); err == nil {
					return nil
				}
				// Config path
				if err := runDeleteConfigmap(cmd.Context(), target, opts); err == nil {
					return nil
				}
				return fmt.Errorf("failed to delete resource '%s' in namespace %s (not found)", target, opts.namespace)
			}
			// For any other error (e.g., dependents exist), return immediately
			return err
		}
		return nil
	}

	// If no args provided, show help
	return cmd.Help()
}

// newDeleteServiceCmd creates the service deletion subcommand
func newDeleteServiceCmd() *cobra.Command {
	opts := &deleteOptions{}

	cmd := &cobra.Command{
		Use:   "service <service-name>",
		Short: "Delete a service and its resources",
		Long: `Delete a service and all its associated resources.

This command provides a safe and controlled way to remove services from the Rune platform.
It ensures proper cleanup of all associated resources while maintaining data integrity.

The deletion process includes:
1. Graceful shutdown of running instances
2. Cleanup of associated resources (volumes, networks, etc.)
3. Deregistration from service discovery
4. Removal from the service registry

By default, the command shows real-time progress and waits for completion.
Use --detach to start deletion in the background and return immediately.

Examples:
  # Delete a service with confirmation (shows real-time progress)
  rune delete service my-service

  # Delete a service without confirmation
  rune delete service my-service --force

  # Delete a service and return immediately (background deletion)
  rune delete service my-service --detach

  # Show what would be deleted without actually deleting
  rune delete service my-service --dry-run

  # Delete a service immediately without graceful shutdown
  rune delete service my-service --now

  # Delete a service with custom grace period
  rune delete service my-service --grace-period 60

  # Delete a service and ignore if it doesn't exist
  rune delete service my-service --ignore-not-found`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.namespace = effectiveCmdNS(opts.namespace)
			return runServiceDelete(cmd.Context(), args[0], opts)
		},
	}

	// Add flags
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace of the service")
	cmd.Flags().BoolVarP(&opts.force, "force", "f", false, "Skip confirmation prompt")
	cmd.Flags().Int32VarP(&opts.timeoutSeconds, "timeout", "t", 30, "Graceful shutdown timeout in seconds")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "Start deletion and return immediately")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Show what would be deleted without actually deleting")
	cmd.Flags().Int32Var(&opts.gracePeriod, "grace-period", 0, "Grace period for graceful shutdown (alternative to --timeout)")
	cmd.Flags().BoolVar(&opts.now, "now", false, "Immediate deletion without graceful shutdown")
	cmd.Flags().BoolVar(&opts.ignoreNotFound, "ignore-not-found", false, "Don't error if service doesn't exist")
	cmd.Flags().StringSliceVar(&opts.finalizers, "finalizers", nil, "Optional finalizers to run")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "text", "Output format (text, json, yaml)")
	cmd.Flags().BoolVar(&opts.noDependencies, "no-dependencies", false, "Ignore dependents and proceed with deletion")

	// Mark mutually exclusive flags
	cmd.MarkFlagsMutuallyExclusive("detach", "now")
	cmd.MarkFlagsMutuallyExclusive("timeout", "grace-period")

	return cmd
}

// listOptions holds the options for the list subcommand
type listOptions struct {
	cmdOptions

	namespace string
	status    string
	output    string
}

// newDeleteListCmd creates the list subcommand
func newDeleteListCmd() *cobra.Command {
	opts := &listOptions{}

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List deletion operations",
		Long: `List deletion operations with optional filtering.

This command shows all deletion operations, both active and completed.
You can filter by namespace and status to narrow down the results.

Examples:
  # List all deletion operations
  rune delete list

  # List deletion operations in a specific namespace
  rune delete list --namespace production

  # List only active deletion operations
  rune delete list --status in_progress

  # List failed deletion operations
  rune delete list --status failed

  # Output as JSON
  rune delete list --output json`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.namespace = effectiveCmdNS(opts.namespace)
			return runDeleteList(cmd.Context(), opts)
		},
	}

	// Add flags
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "", "Filter by namespace (empty for all namespaces)")
	cmd.Flags().StringVarP(&opts.status, "status", "s", "", "Filter by status (in_progress, completed, failed, cancelled)")
	cmd.PersistentFlags().StringVarP(&opts.output, "output", "o", "text", "Output format (text, json, yaml)")

	return cmd
}

// deleteStatusOptions holds the options for the status subcommand
type deleteStatusOptions struct {
	namespace string
	output    string
}

// newDeleteStatusCmd creates the status subcommand
func newDeleteStatusCmd() *cobra.Command {
	opts := &deleteStatusOptions{}

	cmd := &cobra.Command{
		Use:   "status <deletion-id>",
		Short: "Get status of a deletion operation",
		Long: `Get detailed status of a specific deletion operation.

This command shows the current status, progress, and details of a deletion operation.
Use the deletion ID returned from 'rune delete service --detach' or 'rune delete list'.

Examples:
  # Get status of a deletion operation
  rune delete status abc123-def456

  # Get status as JSON
  rune delete status abc123-def456 --output json`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.namespace = effectiveCmdNS(opts.namespace)
			return runDeleteStatus(cmd.Context(), args[0], opts)
		},
	}

	// Add flags
	cmd.PersistentFlags().StringVarP(&opts.output, "output", "o", "text", "Output format (text, json, yaml)")
	cmd.PersistentFlags().StringVarP(&opts.namespace, "namespace", "n", "", "Namespace of the target")

	return cmd
}

// runServiceDelete executes the delete command
func runServiceDelete(ctx context.Context, serviceName string, opts *deleteOptions) error {
	// Validate flags
	if err := validateDeleteFlags(opts); err != nil {
		return err
	}

	// Create API client
	apiClient, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return fmt.Errorf("failed to create API client: %w", err)
	}
	defer apiClient.Close()

	// Create service client
	serviceClient := client.NewServiceClient(apiClient)

	// Use grace period if specified
	if opts.gracePeriod > 0 {
		opts.timeoutSeconds = opts.gracePeriod
	}

	// Handle dry run
	if opts.dryRun {
		return handleDryRun(ctx, serviceClient, serviceName, opts)
	}

	// Handle confirmation unless force is specified
	if !opts.force && !opts.dryRun {
		if err := confirmDeletion(ctx, serviceClient, serviceName, opts.namespace); err != nil {
			return err
		}
	}

	// Create the deletion request
	deleteReq := &generated.DeleteServiceRequest{
		Name:           serviceName,
		Namespace:      opts.namespace,
		Force:          opts.force || opts.noDependencies,
		TimeoutSeconds: opts.timeoutSeconds,
		Detach:         opts.detach,
		DryRun:         opts.dryRun,
		GracePeriod:    opts.gracePeriod,
		Now:            opts.now,
		IgnoreNotFound: opts.ignoreNotFound,
		Finalizers:     opts.finalizers,
	}

	// Execute deletion
	resp, err := serviceClient.DeleteServiceWithRequest(deleteReq)
	if err != nil {
		if opts.ignoreNotFound && strings.Contains(err.Error(), "not found") {
			fmt.Printf("Service %s/%s not found (ignored)\n", opts.namespace, serviceName)
			return nil
		}
		return fmt.Errorf("failed to delete service: %w", err)
	}

	// Convert response to expected format
	deletionResp := &types.DeletionResponse{
		DeletionID: resp.DeletionId,
		Status:     resp.Status.Message,
		Warnings:   resp.Warnings,
		Errors:     resp.Errors,
	}

	// Handle response based on output format
	switch opts.output {
	case "json":
		return outputJSON(deletionResp)
	case "yaml":
		return outputYAML(deletionResp)
	default:
		return handleTextOutput(ctx, serviceClient, deletionResp, opts)
	}
}

// validateDeleteFlags validates the delete command flags
func validateDeleteFlags(opts *deleteOptions) error {
	if opts.timeoutSeconds < 0 {
		return fmt.Errorf("timeout must be non-negative")
	}

	if opts.gracePeriod < 0 {
		return fmt.Errorf("grace-period must be non-negative")
	}

	if opts.now && (opts.timeoutSeconds > 0 || opts.gracePeriod > 0) {
		return fmt.Errorf("--now cannot be used with --timeout or --grace-period")
	}

	validOutputs := []string{"text", "json", "yaml"}
	valid := false
	for _, v := range validOutputs {
		if opts.output == v {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("invalid output format: %s (valid: %s)", opts.output, strings.Join(validOutputs, ", "))
	}

	return nil
}

// confirmDeletion prompts the user for confirmation
func confirmDeletion(ctx context.Context, serviceClient *client.ServiceClient, serviceName, namespace string) error {
	// Get service details for confirmation
	service, err := serviceClient.GetService(namespace, serviceName)
	if err != nil {
		return fmt.Errorf("failed to get service details: %w", err)
	}

	// Display service information
	fmt.Printf("Service: %s/%s\n", service.Namespace, service.Name)
	fmt.Printf("Image: %s\n", service.Image)
	fmt.Printf("Scale: %d\n", service.Scale)
	fmt.Printf("Status: %s\n", service.Status)

	// Prompt for confirmation
	fmt.Print("Are you sure you want to delete this service? (y/N): ")
	var response string
	fmt.Scanln(&response)

	if strings.ToLower(response) != "y" && strings.ToLower(response) != "yes" {
		fmt.Println("Deletion cancelled.")
		os.Exit(0)
	}

	return nil
}

// handleDryRun handles dry run mode
func handleDryRun(ctx context.Context, serviceClient *client.ServiceClient, serviceName string, opts *deleteOptions) error {
	// Create the deletion request with dry run enabled
	deleteReq := &generated.DeleteServiceRequest{
		Name:           serviceName,
		Namespace:      opts.namespace,
		Force:          opts.force,
		TimeoutSeconds: opts.timeoutSeconds,
		Detach:         opts.detach,
		DryRun:         true, // Always true for dry run
		GracePeriod:    opts.gracePeriod,
		Now:            opts.now,
		IgnoreNotFound: opts.ignoreNotFound,
		Finalizers:     opts.finalizers,
	}

	// Execute dry run deletion
	resp, err := serviceClient.DeleteServiceWithRequest(deleteReq)
	if err != nil {
		if opts.ignoreNotFound && strings.Contains(err.Error(), "not found") {
			notFoundResp := map[string]string{
				"status":  "not_found",
				"message": fmt.Sprintf("Service %s/%s not found (would be ignored)", opts.namespace, serviceName),
			}

			// Handle output based on format
			switch opts.output {
			case "json":
				return outputJSON(notFoundResp)
			case "yaml":
				return outputYAML(notFoundResp)
			default:
				fmt.Printf("Service %s/%s not found (would be ignored)\n", opts.namespace, serviceName)
				return nil
			}
		}
		return fmt.Errorf("failed to perform dry run: %w", err)
	}

	// Convert response to expected format (same as runDelete)
	dryRunResp := &types.DeletionResponse{
		DeletionID: resp.DeletionId,
		Status:     resp.Status.Message,
		Warnings:   resp.Warnings,
		Errors:     resp.Errors,
		Finalizers: convertProtoFinalizersToTypes(resp.Finalizers),
	}

	// Handle response based on output format (same pattern as runDelete)
	switch opts.output {
	case "json":
		return outputJSON(dryRunResp)
	case "yaml":
		return outputYAML(dryRunResp)
	default:
		return handleDryRunTextOutput(dryRunResp, opts.namespace, serviceName)
	}
}

// handleDryRunTextOutput handles text output for dry run operations
func handleDryRunTextOutput(dryRunResp *types.DeletionResponse, namespace, serviceName string) error {
	// Text output
	fmt.Printf("Dry run: Service %s/%s would be deleted\n", namespace, serviceName)

	// Show what would be deleted
	if len(dryRunResp.Finalizers) > 0 {
		fmt.Println("\nCleanup operations that would be performed:")
		for i, finalizer := range dryRunResp.Finalizers {
			fmt.Printf("  %d. %s\n", i+1, finalizer.Type)
		}
	}

	// Show warnings if any
	if len(dryRunResp.Warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, warning := range dryRunResp.Warnings {
			fmt.Printf("  - %s\n", warning)
		}
	}

	// Show errors if any
	if len(dryRunResp.Errors) > 0 {
		fmt.Println("\nErrors:")
		for _, err := range dryRunResp.Errors {
			fmt.Printf("  - %s\n", err)
		}
	}

	return nil
}

// handleTextOutput handles text output format
func handleTextOutput(ctx context.Context, serviceClient *client.ServiceClient, resp *types.DeletionResponse, opts *deleteOptions) error {
	fmt.Printf("Deletion initiated for service\n")
	fmt.Printf("Status: %s\n", resp.Status)

	// Display warnings if any
	if len(resp.Warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, warning := range resp.Warnings {
			fmt.Printf("  - %s\n", warning)
		}
	}

	// Display errors if any
	if len(resp.Errors) > 0 {
		fmt.Println("\nErrors:")
		for _, err := range resp.Errors {
			fmt.Printf("  - %s\n", err)
		}
	}

	// Display cleanup operations
	if len(resp.Finalizers) > 0 {
		fmt.Println("\nCleanup operations:")
		for i, finalizer := range resp.Finalizers {
			fmt.Printf("  %d. %s\n", i+1, finalizer.Type)
		}
	}

	// If detach mode, just return
	if opts.detach {
		fmt.Printf("\nDeletion started in background. Use 'rune get deletion %s' to check progress.\n", resp.DeletionID)
		return nil
	}

	// Default behavior: monitor progress in real-time
	return monitorDeletion(ctx, serviceClient, opts.namespace, resp.DeletionID)
}

// monitorDeletion monitors the deletion progress using service watcher
func monitorDeletion(ctx context.Context, serviceClient *client.ServiceClient, namespace, deletionID string) error {
	fmt.Println("\nMonitoring deletion progress...")

	// Get initial deletion status
	statusResp, err := serviceClient.GetDeletionStatus(namespace, deletionID)
	if err != nil {
		return fmt.Errorf("failed to get initial deletion status: %w", err)
	}

	if statusResp.Operation == nil {
		return fmt.Errorf("no operation found for deletion ID %s", deletionID)
	}

	serviceName := statusResp.Operation.ServiceName

	// Debug: Check if serviceName and namespace are populated
	if serviceName == "" {
		return fmt.Errorf("service name is empty in deletion operation")
	}
	if namespace == "" {
		return fmt.Errorf("namespace is empty in deletion operation")
	}

	// Check if deletion is already complete
	if statusResp.Operation.Status == "completed" {
		fmt.Println("✓ Deletion completed successfully")
		return nil
	}

	fmt.Printf("Watching service %s/%s for deletion...\n", namespace, serviceName)

	// Try to get the service to see if it still exists
	service, err := serviceClient.GetService(namespace, serviceName)
	if err != nil {
		// Service not found - deletion already completed
		if strings.Contains(err.Error(), "not found") {
			fmt.Println("✓ Service already deleted")
			return nil
		}
		return fmt.Errorf("failed to check service status: %w", err)
	}

	fmt.Printf("Service still exists (Status: %s), starting watch...\n", service.Status)

	// Start watching the service for changes
	fieldSelector := fmt.Sprintf("name=%s", serviceName)
	eventCh, cancelWatch, err := serviceClient.WatchServices(namespace, "", fieldSelector)
	if err != nil {
		return fmt.Errorf("failed to watch service: %w", err)
	}
	defer cancelWatch() // Ensure we clean up the watch when done

	// Set a timeout to avoid waiting forever
	timeout := time.After(30 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-timeout:
			// Check deletion status one more time before timing out
			statusResp, err := serviceClient.GetDeletionStatus(namespace, deletionID)
			if err == nil && statusResp.Operation.Status == "completed" {
				fmt.Println("✓ Deletion completed successfully")
				return nil
			}
			fmt.Println("⚠ Deletion monitoring timed out after 30 seconds")
			return nil

		case event, ok := <-eventCh:
			if !ok {
				// Channel closed, service watch ended - this likely means the service was deleted
				cancelWatch()
				fmt.Println("✓ Deletion completed successfully")
				return nil
			}

			if event.Error != nil {
				return fmt.Errorf("watch error: %w", event.Error)
			}

			switch event.EventType {
			case "MODIFIED":
				// Service is being modified (probably status updates during deletion)
				fmt.Printf("Service status: %s\n", event.Service.Status)
			case "DELETED":
				// Service has been deleted - cancel watch and return
				cancelWatch()
				fmt.Println("✓ Service deleted successfully")
				return nil
			}
		}
	}
}

// convertProtoFinalizersToTypes converts protobuf finalizers to types finalizers
func convertProtoFinalizersToTypes(protoFinalizers []*generated.Finalizer) []types.Finalizer {
	result := make([]types.Finalizer, len(protoFinalizers))
	for i, protoFinalizer := range protoFinalizers {
		finalizer := types.Finalizer{
			ID:     protoFinalizer.Id,
			Type:   types.FinalizerType(protoFinalizer.Type),
			Status: types.FinalizerStatus(protoFinalizer.Status),
			Error:  protoFinalizer.Error,
		}

		// Convert timestamps
		finalizer.CreatedAt = time.Unix(protoFinalizer.CreatedAt, 0)
		finalizer.UpdatedAt = time.Unix(protoFinalizer.UpdatedAt, 0)
		if protoFinalizer.CompletedAt > 0 {
			completedAt := time.Unix(protoFinalizer.CompletedAt, 0)
			finalizer.CompletedAt = &completedAt
		}

		// Convert dependencies
		if len(protoFinalizer.Dependencies) > 0 {
			finalizer.Dependencies = make([]types.FinalizerDependency, len(protoFinalizer.Dependencies))
			for j, protoDep := range protoFinalizer.Dependencies {
				finalizer.Dependencies[j] = types.FinalizerDependency{
					DependsOn: types.FinalizerType(protoDep.DependsOn),
					Required:  protoDep.Required,
				}
			}
		}

		result[i] = finalizer
	}
	return result
}

// runDeleteList executes the list subcommand
func runDeleteList(ctx context.Context, opts *listOptions) error {
	// Create API client
	apiClient, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return fmt.Errorf("failed to create API client: %w", err)
	}
	defer apiClient.Close()

	// Create service client
	serviceClient := client.NewServiceClient(apiClient)

	// Create the list request
	listReq := &generated.ListDeletionOperationsRequest{
		Namespace: opts.namespace,
		Status:    opts.status,
	}

	// Execute list
	resp, err := serviceClient.ListDeletionOperations(listReq.Namespace, listReq.Status)
	if err != nil {
		return fmt.Errorf("failed to list deletion operations: %w", err)
	}

	// Handle response based on output format
	switch opts.output {
	case "json":
		return outputJSON(resp.Operations)
	case "yaml":
		return outputYAML(resp.Operations)
	default:
		return outputDeleteTable(resp.Operations, opts)
	}
}

// runDeleteStatus executes the status subcommand
func runDeleteStatus(ctx context.Context, deletionID string, opts *deleteStatusOptions) error {
	// Create API client
	apiClient, err := newAPIClient("", "")
	if err != nil {
		return fmt.Errorf("failed to create API client: %w", err)
	}
	defer apiClient.Close()

	// Create service client
	serviceClient := client.NewServiceClient(apiClient)

	// Create the status request
	statusReq := &generated.GetDeletionStatusRequest{
		Namespace: opts.namespace,
		Name:      deletionID,
	}

	// Execute status
	resp, err := serviceClient.GetDeletionStatus(statusReq.Namespace, statusReq.Name)
	if err != nil {
		return fmt.Errorf("failed to get deletion status: %w", err)
	}

	// Handle response based on output format
	switch opts.output {
	case "json":
		return outputJSON(resp.Operation)
	case "yaml":
		return outputYAML(resp.Operation)
	default:
		return outputDeleteStatusText(resp.Operation)
	}
}

// outputDeleteStatusText outputs deletion status in text format
func outputDeleteStatusText(operation *generated.DeletionOperation) error {
	fmt.Printf("Deletion ID: %s\n", operation.Id)
	fmt.Printf("Namespace: %s\n", operation.Namespace)
	fmt.Printf("Service: %s\n", operation.ServiceName)
	fmt.Printf("Status: %s\n", operation.Status)
	fmt.Printf("Start Time: %s\n", time.Unix(operation.StartTime, 0).Format(time.RFC3339))

	if operation.EndTime > 0 {
		fmt.Printf("End Time: %s\n", time.Unix(operation.EndTime, 0).Format(time.RFC3339))
		duration := time.Unix(operation.EndTime, 0).Sub(time.Unix(operation.StartTime, 0))
		fmt.Printf("Duration: %s\n", duration.String())
	}

	fmt.Printf("Progress: %d/%d instances\n", operation.DeletedInstances, operation.TotalInstances)

	if operation.FailedInstances > 0 {
		fmt.Printf("Failed: %d instances\n", operation.FailedInstances)
	}

	if operation.FailureReason != "" {
		fmt.Printf("Failure Reason: %s\n", operation.FailureReason)
	}

	if len(operation.PendingOperations) > 0 {
		fmt.Printf("Pending Operations:\n")
		for _, op := range operation.PendingOperations {
			fmt.Printf("  - %s\n", op)
		}
	}

	if len(operation.Finalizers) > 0 {
		fmt.Printf("Finalizers:\n")
		for _, finalizer := range operation.Finalizers {
			fmt.Printf("  - %s: %s\n", finalizer.Type, finalizer.Status)
			if finalizer.Error != "" {
				fmt.Printf("    Error: %s\n", finalizer.Error)
			}
		}
	}

	return nil
}

func init() { rootCmd.AddCommand(newDeleteCmd()) }

// runDeleteSecret deletes a secret by name using the SecretClient
func runDeleteSecret(ctx context.Context, name string, opts *deleteOptions) error {
	apiClient, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return err
	}
	defer apiClient.Close()
	sc := client.NewSecretClient(apiClient)
	if err := sc.DeleteSecret(opts.namespace, name); err != nil {
		return err
	}
	fmt.Printf("Secret %s/%s deleted\n", opts.namespace, name)
	return nil
}

// runDeleteConfigmap deletes a config by name using the ConfigClient
func runDeleteConfigmap(ctx context.Context, name string, opts *deleteOptions) error {
	apiClient, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return err
	}
	defer apiClient.Close()
	cc := client.NewConfigmapClient(apiClient)
	if err := cc.DeleteConfigmap(opts.namespace, name); err != nil {
		return err
	}
	fmt.Printf("Config %s/%s deleted\n", opts.namespace, name)
	return nil
}

// outputDeleteTable outputs deletion operations in a formatted table
func outputDeleteTable(operations []*generated.DeletionOperation, opts *listOptions) error {
	// Create and configure the table renderer
	table := NewResourceTable()
	table.ShowHeaders = !opts.noHeaders
	table.ShowLabels = opts.showLabels

	// Render the table
	return table.RenderDeletionOperations(operations)
}
