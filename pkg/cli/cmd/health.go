package cmd

import (
	"fmt"
	"strings"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/spf13/cobra"
)

var (
	healthNamespace  string
	includeChecks    bool
	healthClientAddr string
)

// healthCmd represents the health command
var healthCmd = &cobra.Command{
	Use:   "health [target|name] [name]",
	Short: "Show health for services, instances, or nodes",
	Long:  "Display health information for a target: service, instance, or node. If only a single name is provided, it is treated as a service name by default (instance if it looks like an instance ID).",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runHealth,
	Example: `  # Service health (explicit)
  rune health service api -n default --checks

  # Instance health by ID (explicit)
  rune health instance inst-123 -n default

  # Node health (single-node)
  rune health node --checks

  # Short target aliases
  rune health svc api
  rune health inst inst-123

  # Autodetect (single argument): prefers service by name, falls back to instance ID
  rune health api -n default`,
}

func init() {
	rootCmd.AddCommand(healthCmd)
	healthCmd.Flags().StringVarP(&healthNamespace, "namespace", "n", "default", "Namespace for namespaced targets")
	healthCmd.Flags().BoolVar(&includeChecks, "checks", false, "Include detailed check results")
	healthCmd.Flags().StringVar(&healthClientAddr, "api-server", "", "Address of the API server")
}

func runHealth(cmd *cobra.Command, args []string) error {
	target := ""
	name := ""
	if len(args) == 1 {
		// Autodetect: single arg is a name
		name = args[0]
	} else {
		target = strings.ToLower(args[0])
		name = args[1]
	}

	apiClient, err := newAPIClient(healthClientAddr, "")
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	hc := client.NewHealthClient(apiClient)

	var resp *generated.GetHealthResponse
	// If target was specified explicitly, use it
	if target != "" {
		normalized, err := normalizeHealthTarget(target)
		if err != nil {
			return err
		}
		r, err := hc.GetHealth(normalized, name, healthNamespace, includeChecks)
		if err != nil {
			return err
		}
		resp = r
	} else {
		// Autodetect: try service first, then instance
		if r, err := hc.GetHealth("service", name, healthNamespace, includeChecks); err == nil {
			resp = r
		} else {
			if r2, err2 := hc.GetHealth("instance", name, healthNamespace, includeChecks); err2 == nil {
				resp = r2
			} else {
				// Surface the original service error for context
				return fmt.Errorf("health not found for '%s' as service or instance", name)
			}
		}
	}

	// Simple pretty print
	for _, c := range resp.Components {
		ns := c.Namespace
		if ns == "" {
			ns = "-"
		}
		fmt.Printf("%s %s ns=%s status=%s msg=%s\n", c.ComponentType, c.Name, ns, c.Status.String(), c.Message)
		if includeChecks {
			for _, chk := range c.CheckResults {
				fmt.Printf("  - %s: %s\n", chk.Type.String(), chk.Message)
			}
		}
	}

	return nil
}

// normalizeHealthTarget maps aliases to canonical component type
func normalizeHealthTarget(target string) (string, error) {
	switch target {
	case "service", "services", "svc":
		return "service", nil
	case "instance", "instances", "inst":
		return "instance", nil
	case "node", "nodes":
		return "node", nil
	default:
		return "", fmt.Errorf("unsupported target: %s (use service|instance|node)", target)
	}
}

// no custom wrapper needed; we use generated types directly
