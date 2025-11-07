package cmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

type cmdOptions struct {
	namespace       string
	addressOverride string
	tokenOverride   string
	showLabels      bool
	noHeaders       bool
}

type resolvedResourceTarget struct {
	namespace  string
	targetType types.ResourceType
	target     string
}

// effectiveCmdNS returns effective namespace for the command, if not provided,
// the default namespace for the current context  else "" is returned
func effectiveCmdNS(namespace string) string {
	// Set Namespace
	if namespace != "" {
		return namespace
	}

	defaultNamespace := viper.GetString("contexts.default.defaultNamespace")
	if defaultNamespace != "" {
		return defaultNamespace
	}

	return ""
}

func parseTargetExpression(targetExpression string) types.ResourceType {
	if strings.Contains(targetExpression, "instance/") || strings.Contains(targetExpression, "inst/") {
		return types.ResourceTypeInstance
	}

	if strings.Contains(targetExpression, "service/") || strings.Contains(targetExpression, "svc/") {
		return types.ResourceTypeService
	}

	return ""
}

func resolveResourceTarget(apiClient *client.Client, targetExpression string, namespace string) (*resolvedResourceTarget, error) {

	targetType := parseTargetExpression(targetExpression)

	switch targetType {
	case types.ResourceTypeInstance:
		return resolveTargetInstance(apiClient, targetExpression, namespace)
	case types.ResourceTypeService:
		return resolveTargetService(apiClient, targetExpression, namespace)
	}

	// try to resolve as a service
	serviceTarget, err := resolveTargetService(apiClient, targetExpression, namespace)
	if err == nil {
		return serviceTarget, nil
	}

	// try to resolve as an instance if it's not a service
	instanceTarget, err := resolveTargetInstance(apiClient, targetExpression, namespace)
	if err == nil {
		return instanceTarget, nil
	}

	return nil, fmt.Errorf("failed to resolve resource target")
}

func resolveTargetService(apiClient *client.Client, target string, namespace string) (*resolvedResourceTarget, error) {

	serviceClient := client.NewServiceClient(apiClient)
	_, err := serviceClient.GetService(namespace, target)
	if err != nil {
		return nil, err
	}

	return &resolvedResourceTarget{targetType: types.ResourceTypeService, target: target, namespace: namespace}, nil
}

func resolveTargetInstance(apiClient *client.Client, target string, namespace string) (*resolvedResourceTarget, error) {
	instanceClient := client.NewInstanceClient(apiClient)
	_, err := instanceClient.GetInstance(namespace, target)
	if err != nil {
		return nil, err
	}

	return &resolvedResourceTarget{targetType: types.ResourceTypeInstance, target: target, namespace: namespace}, nil
}

// buildClientOptions builds ClientOptions using current context from viper and an optional address override.
func buildClientOptions(addressOverride string) *client.ClientOptions {
	opts := client.DefaultClientOptions()
	// Address precedence: explicit override > context server (normalized) > default
	if addressOverride != "" {
		opts.Address = ensureDefaultGRPCPort(addressOverride)
	} else {
		if s := viper.GetString("contexts.default.server"); s != "" {
			opts.Address = ensureDefaultGRPCPort(s)
		}
	}
	// Token from context/env
	if t := viper.GetString("contexts.default.token"); t != "" {
		opts.Token = t
	} else if t, ok := getEnv("RUNE_TOKEN"); ok {
		opts.Token = t
	}
	return opts
}

// newAPIClient creates a new client using buildClientOptions with optional address and token overrides.
func newAPIClient(addressOverride string, tokenOverride string) (*client.Client, error) {
	opts := buildClientOptions(addressOverride)
	if tokenOverride != "" {
		opts.Token = tokenOverride
	}
	return client.NewClient(opts)
}

// newDefaultClient creates a new client using buildClientOptions with optional address and token overrides.
func createAPIClient(opts *cmdOptions) (*client.Client, error) {
	clientOpts := buildClientOptions(opts.addressOverride)
	if opts.tokenOverride != "" {
		clientOpts.Token = opts.tokenOverride
	}
	return client.NewClient(clientOpts)
}

// resolveResourceType resolves a resource type from user input to a canonical type
func resolveResourceType(input string) (string, error) {
	// Lookup the resource type alias
	if resourceType, ok := resourceAliases[input]; ok {
		return resourceType, nil
	}

	return "", fmt.Errorf("unsupported resource type: %s", input)
}

// ensureDefaultGRPCPort ensures a server address includes a port, defaulting to types.DefaultGRPCPort for gRPC.
// Accepts either a bare host or an http/https URL; returns host:port without scheme.
// Examples:
//   - https://prod.example.com -> prod.example.com:7863
//   - example.com -> example.com:7863
//   - host:9000 -> host:9000 (unchanged)
func ensureDefaultGRPCPort(server string) string {
	if server == "" {
		return server
	}
	host := server
	if u, err := url.Parse(server); err == nil && u.Host != "" {
		host = u.Host
	}
	if _, _, err := netSplitHostPortSafe(host); err == nil {
		return host
	}
	if !strings.Contains(host, ":") {
		return fmt.Sprintf("%s:%d", host, config.DefaultGRPCPort)
	}
	return host
}

func getTargetRuneServer() string {
	// Determine target display name
	var targetDisplay string
	if s := viper.GetString("contexts.default.server"); s != "" {
		targetDisplay = ensureDefaultGRPCPort(s)
	} else {
		return fmt.Sprintf("localhost:%d", config.DefaultGRPCPort)
	}
	return targetDisplay
}

// outputResource outputs a resource in the specified format
func outputResource(resources interface{}, opts *getOptions) error {
	switch opts.outputFormat {
	case "json":
		return outputJSON(resources)
	case "yaml":
		return outputYAML(resources)
	case "table", "":
		return outputTable(resources, opts)
	default:
		return fmt.Errorf("unsupported output format: %s", opts.outputFormat)
	}
}

// outputTable outputs resources in table format
func outputTable(resources interface{}, opts *getOptions) error {
	switch r := resources.(type) {
	case []*types.Service:
		return outputServicesTable(r, opts)
	case []*types.Instance:
		return outputInstancesTable(r, opts)
	case []*types.Namespace:
		return outputNamespacesTable(r, opts)
	case []*types.Secret:
		return outputSecretsTable(r, opts)
	case []*types.Configmap:
		return outputConfigmapsTable(r, opts)
	// TODO: Add more resource types here
	default:
		return fmt.Errorf("unsupported resource type for table output")
	}
}

// outputJSON outputs resources in JSON format
func outputJSON(resources interface{}) error {
	jsonData, err := json.MarshalIndent(resources, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal resources to JSON: %w", err)
	}
	fmt.Println(string(jsonData))
	return nil
}

// outputYAML outputs resources in YAML format
func outputYAML(resources interface{}) error {
	yamlData, err := yaml.Marshal(resources)
	if err != nil {
		return fmt.Errorf("failed to marshal resources to YAML: %w", err)
	}
	fmt.Println(string(yamlData))
	return nil
}

// netSplitHostPortSafe tries to split host:port but tolerates missing port
func netSplitHostPortSafe(hostport string) (host, port string, err error) {
	if strings.HasPrefix(hostport, "[") {
		// IPv6 literal
		if i := strings.LastIndex(hostport, "]"); i != -1 {
			host = hostport[:i+1]
			rest := hostport[i+1:]
			if strings.HasPrefix(rest, ":") {
				port = rest[1:]
				if port == "" {
					return "", "", fmt.Errorf("missing port")
				}
				return host, port, nil
			}
			return host, "", fmt.Errorf("no port")
		}
	}
	if i := strings.LastIndex(hostport, ":"); i != -1 && !strings.Contains(hostport[i+1:], ":") {
		return hostport[:i], hostport[i+1:], nil
	}
	return "", "", fmt.Errorf("no port")
}

// formatAge formats a time.Time as a human-readable age string
func formatAge(t time.Time) string {
	if t.IsZero() {
		return "Unknown"
	}

	duration := time.Since(t)
	if duration < time.Minute {
		return "Just now"
	} else if duration < time.Hour {
		minutes := int(duration.Minutes())
		return fmt.Sprintf("%dm", minutes)
	} else if duration < 24*time.Hour {
		hours := int(duration.Hours())
		return fmt.Sprintf("%dh", hours)
	} else if duration < 30*24*time.Hour {
		days := int(duration.Hours() / 24)
		return fmt.Sprintf("%dd", days)
	} else if duration < 365*24*time.Hour {
		months := int(duration.Hours() / 24 / 30)
		return fmt.Sprintf("%dmo", months)
	}
	years := int(duration.Hours() / 24 / 365)
	return fmt.Sprintf("%dy", years)
}
