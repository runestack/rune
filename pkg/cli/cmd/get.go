package cmd

import (
	"context"
	"fmt"
	"sort"

	"github.com/runestack/rune/pkg/api/client"
	resourceWatcher "github.com/runestack/rune/pkg/cli/watcher"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

type getOptions struct {
	cmdOptions

	// Get command flags
	allNamespaces  bool
	outputFormat   string
	watchResources bool
	labelSelector  string
	fieldSelector  string
	sortBy         string
	limit          int
	watchTimeout   string
	serviceName    string
}

// Resource type abbreviations
var resourceAliases = map[string]string{
	// Full names
	"service":    "service",
	"services":   "service",
	"instance":   "instance",
	"instances":  "instance",
	"namespace":  "namespace",
	"namespaces": "namespace",
	"job":        "job",
	"jobs":       "job",
	"config":     "config",
	"configs":    "config",
	"secret":     "secret",
	"secrets":    "secret",
	// Abbreviations
	"svc":        "service",
	"inst":       "instance",
	"ns":         "namespace",
	"cfg":        "config",
	"configmap":  "configmap",
	"configmaps": "configmap",
}

// getCmd represents the get command
func newGetCmd() *cobra.Command {
	opts := &getOptions{}
	cmd := &cobra.Command{
		Use:   "get <resource-type> [resource-name] [flags]",
		Short: "Display one or many resources",
		Long: `Display one or many resources.
	The resource types include:
	* services (svc)
	* instances (inst)
	* namespaces (ns)
	* jobs
	* configs
	* secrets`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.namespace = effectiveCmdNS(opts.namespace)
			return runGet(cmd, args, opts)
		},
		Example: `  # List all services in the current namespace
	rune get services
	
	# List a specific service in YAML format
	rune get service my-service --output=yaml
	
	# List all services in all namespaces
	rune get services --all-namespaces
	
	# List services with real-time updates
	rune get services --watch
	
	# List services filtered by labels
	rune get services --selector=app=backend`,
	}

	// Local flags for the get command
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "", "Namespace to list resources from")
	cmd.Flags().BoolVarP(&opts.allNamespaces, "all-namespaces", "A", false, "List resources across all namespaces")
	cmd.Flags().StringVarP(&opts.outputFormat, "output", "o", "table", "Output format (table, json, yaml)")
	cmd.Flags().BoolVarP(&opts.watchResources, "watch", "w", false, "Watch for changes and update in real-time")
	cmd.Flags().StringVarP(&opts.labelSelector, "selector", "l", "", "Filter resources by label selector (e.g., app=frontend)")
	cmd.Flags().StringVar(&opts.fieldSelector, "field-selector", "", "Filter resources by field selector (e.g., status=running)")
	cmd.Flags().StringVar(&opts.sortBy, "sort-by", "name", "Sort resources by field (name, creationTime, status)")
	cmd.Flags().BoolVar(&opts.showLabels, "show-labels", false, "Show labels as the last column (table output only)")
	cmd.Flags().BoolVar(&opts.noHeaders, "no-headers", false, "Don't print headers for table output")
	cmd.Flags().IntVar(&opts.limit, "limit", 0, "Maximum number of resources to list (0 for unlimited)")
	cmd.Flags().StringVar(&opts.watchTimeout, "timeout", "", "Timeout for watch operations (e.g., 30s, 5m, 1h) - default is no timeout")
	cmd.Flags().StringVar(&opts.serviceName, "service-name", "", "Filter instances by service name")

	// Remove api-key flag; token comes from config/env
	cmd.Flags().StringVar(&opts.addressOverride, "api-server", "", "Address of the API server")

	return cmd
}

func init() { rootCmd.AddCommand(newGetCmd()) }

// runGet is the main entry point for the get command
func runGet(cmd *cobra.Command, args []string, opts *getOptions) error {
	// Resolve resource type from the first argument
	resourceType, err := resolveResourceType(args[0])
	if err != nil {
		return err
	}

	// Get resource name if provided
	var resourceName string
	if len(args) > 1 {
		resourceName = args[1]
	}

	// Create API client
	apiClient, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	// Process the request based on resource type
	switch resourceType {
	case "service":
		return handleServiceGet(cmd, opts, resourceName)
	case "instance":
		return handleInstanceGet(cmd, opts, resourceName)
	case "namespace":
		return handleNamespaceGet(cmd, opts, resourceName)
	case "ns":
		return handleNamespaceGet(cmd, opts, resourceName)
	case "secret":
		return handleSecretGet(cmd, opts, resourceName)
	case "configmap":
		return handleConfigmapGet(cmd, opts, resourceName)
	default:
		return fmt.Errorf("unsupported resource type: %s", args[0])
	}
}

// handleServiceGet handles get operations for services
func handleServiceGet(cmd *cobra.Command, opts *getOptions, resourceName string) error {
	apiClient, cErr := createAPIClient(&opts.cmdOptions)
	if cErr != nil {
		return fmt.Errorf("failed to create API client: %w", cErr)
	}
	defer apiClient.Close()

	serviceClient := client.NewServiceClient(apiClient)

	// If watch mode is enabled, handle watching
	if opts.watchResources {
		return watchServices(cmd.Context(), serviceClient, resourceName, opts)
	}

	// If a specific service name is provided, get that service
	if resourceName != "" {
		service, err := serviceClient.GetService(opts.namespace, resourceName)
		if err != nil {
			return fmt.Errorf("failed to get service %s: %w", resourceName, err)
		}

		return outputResource([]*types.Service{service}, opts)
	}

	// Otherwise, list services based on the namespace flag
	var services []*types.Service
	var err error

	if opts.allNamespaces {
		// List services across all namespaces using the asterisk wildcard
		services, err = serviceClient.ListServices("*", opts.labelSelector, opts.fieldSelector)
		if err != nil {
			return fmt.Errorf("failed to list services across all namespaces: %w", err)
		}
	} else {
		services, err = serviceClient.ListServices(opts.namespace, opts.labelSelector, opts.fieldSelector)
		if err != nil {
			return fmt.Errorf("failed to list services: %w", err)
		}
	}

	// Sort services (client-side sorting is still fine)
	sortServices(services, opts.sortBy)

	// Apply limit if specified
	if opts.limit > 0 && len(services) > opts.limit {
		services = services[:opts.limit]
	}

	return outputResource(services, opts)
}

// handleInstanceGet handles get operations for instances
func handleInstanceGet(cmd *cobra.Command, opts *getOptions, resourceName string) error {
	apiClient, cErr := createAPIClient(&opts.cmdOptions)
	if cErr != nil {
		return fmt.Errorf("failed to create API client: %w", cErr)
	}
	defer apiClient.Close()

	instanceClient := client.NewInstanceClient(apiClient)

	// If watch mode is enabled, handle watching
	if opts.watchResources {
		return watchInstances(cmd.Context(), instanceClient, resourceName, opts)
	}

	// If a specific instance name is provided, get that instance
	if resourceName != "" {
		namespace := opts.namespace
		instance, err := instanceClient.GetInstance(namespace, resourceName)
		if err != nil {
			return fmt.Errorf("failed to get instance: %w", err)
		}

		// Render a single instance
		if opts.outputFormat == "" {
			// Create table for a single instance
			table := NewResourceTable()
			table.RenderInstances([]*types.Instance{instance})
			return nil
		}

		// Handle JSON/YAML output for a single instance
		return outputResource([]*types.Instance{instance}, opts)
	}

	// Get all instances with optional filtering
	instances, err := instanceClient.ListInstances(opts.namespace, opts.serviceName, opts.labelSelector, opts.fieldSelector)
	if err != nil {
		return fmt.Errorf("failed to list instances: %w", err)
	}

	// Render the instances
	if opts.outputFormat == "" {
		// Create table with configured options
		table := NewResourceTable()
		table.AllNamespaces = opts.allNamespaces
		table.ShowLabels = opts.showLabels
		table.RenderInstances(instances)
		return nil
	}

	// Handle JSON/YAML output
	return outputResource(instances, opts)
}

// handleNamespaceGet handles get operations for namespaces
func handleNamespaceGet(cmd *cobra.Command, opts *getOptions, resourceName string) error {
	apiClient, cErr := createAPIClient(&opts.cmdOptions)
	if cErr != nil {
		return fmt.Errorf("failed to create API client: %w", cErr)
	}
	defer apiClient.Close()

	namespaceClient := client.NewNamespaceClient(apiClient)

	// If watch mode is enabled, handle watching
	if opts.watchResources {
		return watchNamespaces(cmd.Context(), namespaceClient, resourceName, opts)
	}

	// If a specific namespace name is provided, get that namespace
	if resourceName != "" {
		namespace, err := namespaceClient.GetNamespace(resourceName)
		if err != nil {
			return fmt.Errorf("failed to get namespace %s: %w", resourceName, err)
		}

		return outputResource([]*types.Namespace{namespace}, opts)
	}

	// Otherwise, list all namespaces
	namespaces, err := namespaceClient.ListNamespaces(nil, nil)
	if err != nil {
		return fmt.Errorf("failed to list namespaces: %w", err)
	}

	// Sort namespaces
	sortNamespaces(namespaces, opts.sortBy)

	// Apply limit if specified
	if opts.limit > 0 && len(namespaces) > opts.limit {
		namespaces = namespaces[:opts.limit]
	}

	return outputResource(namespaces, opts)
}

// handleSecretGet handles get operations for secrets
func handleSecretGet(cmd *cobra.Command, opts *getOptions, resourceName string) error {
	apiClient, cErr := createAPIClient(&opts.cmdOptions)
	if cErr != nil {
		return fmt.Errorf("failed to create API client: %w", cErr)
	}
	defer apiClient.Close()

	secretClient := client.NewSecretClient(apiClient)

	// If a specific secret name is provided, get that secret
	if resourceName != "" {
		namespace := opts.namespace
		secret, err := secretClient.GetSecret(namespace, resourceName)
		if err != nil {
			return fmt.Errorf("failed to get secret %s: %w", resourceName, err)
		}

		return outputResource([]*types.Secret{secret}, opts)
	}

	// Otherwise, list secrets based on the namespace flag
	var secrets []*types.Secret
	var err error

	if opts.allNamespaces {
		// List secrets across all namespaces using the asterisk wildcard
		secrets, err = secretClient.ListSecrets("*", opts.labelSelector, opts.fieldSelector)
		if err != nil {
			return fmt.Errorf("failed to list secrets across all namespaces: %w", err)
		}
	} else {
		secrets, err = secretClient.ListSecrets(opts.namespace, opts.labelSelector, opts.fieldSelector)
		if err != nil {
			return fmt.Errorf("failed to list secrets: %w", err)
		}
	}

	// Apply limit if specified
	if opts.limit > 0 && len(secrets) > opts.limit {
		secrets = secrets[:opts.limit]
	}

	return outputResource(secrets, opts)
}

// handleConfigmapGet handles get operations for configs
func handleConfigmapGet(cmd *cobra.Command, opts *getOptions, resourceName string) error {
	apiClient, cErr := createAPIClient(&opts.cmdOptions)
	if cErr != nil {
		return fmt.Errorf("failed to create API client: %w", cErr)
	}
	defer apiClient.Close()

	configmapClient := client.NewConfigmapClient(apiClient)

	// If a specific config name is provided, get that config
	if resourceName != "" {
		config, err := configmapClient.GetConfigmap(opts.namespace, resourceName)
		if err != nil {
			return fmt.Errorf("failed to get config %s: %w", resourceName, err)
		}

		return outputResource([]*types.Configmap{config}, opts)
	}

	// Otherwise, list configmaps based on the namespace flag
	var configmaps []*types.Configmap
	var err error

	if opts.allNamespaces {
		// List configs across all namespaces using the asterisk wildcard
		configmaps, err = configmapClient.ListConfigmaps("*", opts.labelSelector, opts.fieldSelector)
		if err != nil {
			return fmt.Errorf("failed to list configs across all namespaces: %w", err)
		}
	} else {
		configmaps, err = configmapClient.ListConfigmaps(opts.namespace, opts.labelSelector, opts.fieldSelector)
		if err != nil {
			return fmt.Errorf("failed to list configs: %w", err)
		}
	}

	// Apply limit if specified
	if opts.limit > 0 && len(configmaps) > opts.limit {
		configmaps = configmaps[:opts.limit]
	}

	return outputResource(configmaps, opts)
}

// watchServices watches services for changes
func watchServices(ctx context.Context, serviceClient resourceWatcher.ServiceWatcher, resourceName string, opts *getOptions) error {
	// Create and configure the resource watcher
	watcher := resourceWatcher.NewResourceWatcher()
	watcher.Namespace = opts.namespace
	watcher.AllNamespaces = opts.allNamespaces
	watcher.ResourceName = resourceName
	watcher.LabelSelector = opts.labelSelector
	watcher.FieldSelector = opts.fieldSelector
	watcher.ShowHeaders = !opts.noHeaders

	// Set timeout if specified
	if opts.watchTimeout != "" {
		if err := watcher.SetTimeout(opts.watchTimeout); err != nil {
			return err
		}
	}

	// Use default renderers for services
	watcher.SetResourceToRowsRenderer(resourceWatcher.DefaultServiceResourceToRows)
	watcher.SetEventRenderer(resourceWatcher.DefaultServiceEventRenderer)

	// Create adapter to convert ServiceWatcher to ResourceToWatch
	adapter := &resourceWatcher.ServiceWatcherAdapter{ServiceWatcher: serviceClient}

	// Start watching
	return watcher.Watch(ctx, adapter)
}

// watchNamespaces watches namespaces for changes
func watchNamespaces(ctx context.Context, namespaceClient *client.NamespaceClient, resourceName string, opts *getOptions) error {
	// For now, implement basic watching using the client
	watchCh, err := namespaceClient.WatchNamespaces(nil, nil)
	if err != nil {
		return fmt.Errorf("failed to watch namespaces: %w", err)
	}

	// Print initial namespaces
	namespaces, err := namespaceClient.ListNamespaces(nil, nil)
	if err != nil {
		return fmt.Errorf("failed to list initial namespaces: %w", err)
	}

	// Create table for display
	table := NewResourceTable()
	table.ShowHeaders = !opts.noHeaders
	table.ShowLabels = opts.showLabels

	// Display initial state
	if len(namespaces) > 0 {
		table.RenderNamespaces(namespaces)
	}

	fmt.Println("Watching namespaces... (press Ctrl+C to stop)")

	// Watch for changes
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-watchCh:
			if !ok {
				return nil
			}
			// For now, just re-list and display
			namespaces, err := namespaceClient.ListNamespaces(nil, nil)
			if err != nil {
				fmt.Printf("Error listing namespaces: %v\n", err)
				continue
			}
			// Clear screen and re-render (simple approach)
			fmt.Print("\033[2J\033[H") // Clear screen
			table.RenderNamespaces(namespaces)
		}
	}
}

// watchInstances watches instances for changes
func watchInstances(ctx context.Context, instanceClient resourceWatcher.InstanceWatcher, resourceName string, opts *getOptions) error {
	// Create and configure the resource watcher
	watcher := resourceWatcher.NewResourceWatcher()
	watcher.Namespace = opts.namespace
	watcher.AllNamespaces = opts.allNamespaces
	watcher.ResourceName = resourceName
	watcher.LabelSelector = opts.labelSelector
	watcher.FieldSelector = opts.fieldSelector
	watcher.ShowHeaders = !opts.noHeaders

	// Set timeout if specified
	if opts.watchTimeout != "" {
		if err := watcher.SetTimeout(opts.watchTimeout); err != nil {
			return err
		}
	}

	// Use default renderers for instances
	watcher.SetResourceToRowsRenderer(resourceWatcher.DefaultInstanceResourceToRows)
	watcher.SetEventRenderer(resourceWatcher.DefaultInstanceEventRenderer)

	// Create adapter to convert InstanceWatcher to ResourceToWatch
	adapter := &resourceWatcher.InstanceWatcherAdapter{InstanceWatcher: instanceClient}

	// Start watching
	return watcher.Watch(ctx, adapter)
}

// outputServicesTable outputs services in a formatted table
func outputServicesTable(services []*types.Service, opts *getOptions) error {
	// Create and configure the table renderer
	table := NewResourceTable()
	table.AllNamespaces = opts.allNamespaces
	table.ShowHeaders = !opts.noHeaders
	table.ShowLabels = opts.showLabels

	// Render the table
	return table.RenderServices(services)
}

// outputInstancesTable outputs instances in a formatted table
func outputInstancesTable(instances []*types.Instance, opts *getOptions) error {
	// Create and configure the table renderer
	table := NewResourceTable()
	table.AllNamespaces = opts.allNamespaces
	table.ShowHeaders = !opts.noHeaders
	table.ShowLabels = opts.showLabels

	// Render the table
	return table.RenderInstances(instances)
}

// outputNamespacesTable outputs namespaces in a formatted table
func outputNamespacesTable(namespaces []*types.Namespace, opts *getOptions) error {
	// Create and configure the table renderer
	table := NewResourceTable()
	table.ShowHeaders = !opts.noHeaders
	table.ShowLabels = opts.showLabels

	// Render the table
	return table.RenderNamespaces(namespaces)
}

// outputSecretsTable outputs secrets in a formatted table
func outputSecretsTable(secrets []*types.Secret, opts *getOptions) error {
	// Create and configure the table renderer
	table := NewResourceTable()
	table.ShowHeaders = !opts.noHeaders
	table.ShowLabels = opts.showLabels

	// Render the table
	return table.RenderSecrets(secrets)
}

// outputConfigmapsTable outputs configmaps in a formatted table
func outputConfigmapsTable(configmaps []*types.Configmap, opts *getOptions) error {
	// Create and configure the table renderer
	table := NewResourceTable()
	table.ShowHeaders = !opts.noHeaders
	table.ShowLabels = opts.showLabels

	// Render the table
	return table.RenderConfigmaps(configmaps)
}

// sortServices sorts services based on the specified sort field
func sortServices(services []*types.Service, sortField string) {
	switch sortField {
	case "name":
		sort.Slice(services, func(i, j int) bool {
			return services[i].Name < services[j].Name
		})
	case "creationTime", "age":
		sort.Slice(services, func(i, j int) bool {
			return services[i].Metadata.CreatedAt.Before(services[j].Metadata.CreatedAt)
		})
	case "status":
		sort.Slice(services, func(i, j int) bool {
			return string(services[i].Status) < string(services[j].Status)
		})
	}
}

// sortNamespaces sorts namespaces based on the specified sort field
func sortNamespaces(namespaces []*types.Namespace, sortField string) {
	switch sortField {
	case "name":
		sort.Slice(namespaces, func(i, j int) bool {
			return namespaces[i].Name < namespaces[j].Name
		})
	case "creationTime", "age":
		sort.Slice(namespaces, func(i, j int) bool {
			return namespaces[i].CreatedAt.Before(namespaces[j].CreatedAt)
		})
	}
}
