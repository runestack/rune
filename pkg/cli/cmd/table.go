package cmd

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pterm/pterm"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/types"
)

// ResourceTable provides a generic interface for rendering tables of different resources
type ResourceTable struct {
	// Configuration
	Headers       []string
	ShowHeaders   bool
	AllNamespaces bool
	ShowLabels    bool
	MaxWidth      int

	// Rendering details
	tableRenderer *pterm.TablePrinter
	stripAnsiFunc func(string) string
}

// NewResourceTable creates a new resource table with default configuration
func NewResourceTable() *ResourceTable {
	// Create the table with custom header style
	table := pterm.DefaultTable.WithHasHeader(true)

	// Customize the header style to use BoldBlue
	headerStyle := pterm.NewStyle(pterm.FgCyan, pterm.Bold)
	table = table.WithHeaderStyle(headerStyle)

	return &ResourceTable{
		ShowHeaders:   true,
		stripAnsiFunc: stripAnsiTable,
		tableRenderer: table,
		MaxWidth:      100,
	}
}

// SetHeaders sets the headers for the table
func (t *ResourceTable) SetHeaders(headers []string) {
	t.Headers = headers
}

// SetStripAnsiFunc sets a custom function for stripping ANSI codes
func (t *ResourceTable) SetStripAnsiFunc(fn func(string) string) {
	t.stripAnsiFunc = fn
}

// RenderServices renders a table of services
func (t *ResourceTable) RenderServices(services []*types.Service) error {
	if len(services) == 0 {
		fmt.Println("No services found")
		return nil
	}

	// Set default headers if not provided
	if len(t.Headers) == 0 {
		if t.AllNamespaces {
			t.Headers = []string{"NAMESPACE", "NAME", "TYPE", "STATUS", "INSTANCES", "EXTERNAL", "GENERATION", "AGE"}
		} else {
			t.Headers = []string{"NAME", "TYPE", "STATUS", "INSTANCES", "EXTERNAL", "GENERATION", "AGE"}
		}
	}

	// Create rows
	rows := [][]string{t.Headers} // Start with headers

	// Generate data rows
	for _, service := range services {
		// Determine service type
		serviceType := "container"
		if service.Runtime == "process" && service.Process != nil {
			serviceType = "process"
		}

		// Format status - use our colorizeStatus function
		status := format.PTermStatusLabel(string(service.Status))

		// Format instances
		running := 0
		if service.Status == types.ServiceStatusRunning {
			running = service.Scale
		}
		instances := fmt.Sprintf("%d/%d", running, service.Scale)

		// External endpoint (best-effort)
		external := "-"
		if service.Expose != nil {
			var portNum int
			for i := range service.Ports {
				p := service.Ports[i]
				if p.Name == service.Expose.Port {
					portNum = p.Port
					break
				}
				if n, err := strconv.Atoi(service.Expose.Port); err == nil && n == p.Port {
					portNum = p.Port
					break
				}
			}
			if portNum > 0 {
				host := service.Expose.Host
				if host == "" {
					host = "localhost"
				}
				hostPort := portNum
				if service.Expose.HostPort > 0 {
					hostPort = service.Expose.HostPort
				}
				external = fmt.Sprintf("http://%s:%d", host, hostPort)
			}
		}

		// Calculate age
		var age, generation string
		if service.Metadata != nil {
			age = formatAgeTable(service.Metadata.CreatedAt)

			// Format generation
			generation = "0"
			if service.Metadata != nil && service.Metadata.Generation > 0 {
				generation = fmt.Sprintf("%d", service.Metadata.Generation)
			}

		}

		// Create the row
		var row []string
		if t.AllNamespaces {
			row = []string{
				service.Namespace,
				service.Name,
				serviceType,
				status,
				instances,
				external,
				generation,
				age,
			}
		} else {
			row = []string{
				service.Name,
				serviceType,
				status,
				instances,
				external,
				generation,
				age,
			}
		}

		// Add labels if requested
		if t.ShowLabels && len(service.Labels) > 0 {
			labelStrs := make([]string, 0, len(service.Labels))
			for k, v := range service.Labels {
				labelStrs = append(labelStrs, fmt.Sprintf("%s=%s", k, v))
			}
			row = append(row, strings.Join(labelStrs, ","))
		}

		rows = append(rows, row)
	}

	// Render the table with pterm
	return t.tableRenderer.WithData(rows).Render()
}

// RenderInstances renders a table of instances
func (t *ResourceTable) RenderInstances(instances []*types.Instance) error {
	if len(instances) == 0 {
		fmt.Println("No instances found")
		return nil
	}

	// Set default headers if not provided
	if len(t.Headers) == 0 {
		if t.AllNamespaces {
			t.Headers = []string{"NAMESPACE", "NAME", "INSTANCE ID", "SERVICE", "NODE", "STATUS", "RESTARTS", "AGE"}
		} else {
			t.Headers = []string{"NAME", "INSTANCE ID", "SERVICE", "NODE", "STATUS", "RESTARTS", "AGE"}
		}
	}

	// Create rows
	rows := [][]string{t.Headers} // Start with headers

	// Generate data rows
	for _, instance := range instances {
		// Format status using PTermStatusLabel
		status := format.PTermStatusLabel(string(instance.Status))

		// Format restarts from instance metadata
		restarts := "0"
		if instance.Metadata != nil {
			restarts = fmt.Sprintf("%d", instance.Metadata.RestartCount)
		}

		// Calculate age
		age := formatAgeTable(instance.CreatedAt)

		// Create the row
		var row []string
		if t.AllNamespaces {
			row = []string{
				instance.Namespace,
				instance.Name,
				instance.ID,
				instance.ServiceName,
				instance.NodeID,
				status,
				restarts,
				age,
			}
		} else {
			row = []string{
				instance.Name,
				instance.ID,
				instance.ServiceName,
				instance.NodeID,
				status,
				restarts,
				age,
			}
		}

		rows = append(rows, row)
	}

	// Render the table with pterm
	return t.tableRenderer.WithData(rows).Render()
}

// RenderNamespaces renders a table of namespaces
func (t *ResourceTable) RenderNamespaces(namespaces []*types.Namespace) error {
	if len(namespaces) == 0 {
		fmt.Println("No namespaces found")
		return nil
	}

	// Set default headers if not provided
	if len(t.Headers) == 0 {
		t.Headers = []string{"NAME", "AGE", "LABELS"}
	}

	// Create rows
	rows := [][]string{t.Headers} // Start with headers

	// Generate data rows
	for _, namespace := range namespaces {
		// Format age
		age := formatAgeTable(namespace.CreatedAt)

		// Format labels
		labels := ""
		if len(namespace.Labels) > 0 {
			labelPairs := make([]string, 0, len(namespace.Labels))
			for k, v := range namespace.Labels {
				labelPairs = append(labelPairs, fmt.Sprintf("%s=%s", k, v))
			}
			labels = strings.Join(labelPairs, ",")
		}

		row := []string{
			namespace.Name,
			age,
			labels,
		}
		rows = append(rows, row)
	}

	// Render the table with pterm
	return t.tableRenderer.WithData(rows).Render()
}

// RenderDeletionOperations renders a table of deletion operations
func (t *ResourceTable) RenderDeletionOperations(operations []*generated.DeletionOperation) error {
	if len(operations) == 0 {
		fmt.Println("No deletion operations found")
		return nil
	}

	// Set default headers if not provided
	if len(t.Headers) == 0 {
		t.Headers = []string{"ID", "NAMESPACE", "SERVICE", "STATUS", "PROGRESS"}
	}

	// Create rows
	rows := [][]string{t.Headers} // Start with headers

	// Generate data rows
	for _, operation := range operations {
		progress := fmt.Sprintf("%d/%d", operation.DeletedInstances, operation.TotalInstances)
		if operation.TotalInstances == 0 {
			progress = "N/A"
		}

		row := []string{
			operation.Id,
			operation.Namespace,
			operation.ServiceName,
			operation.Status,
			progress,
		}
		rows = append(rows, row)
	}

	// Render the table with pterm
	return t.tableRenderer.WithData(rows).Render()
}

// RenderSecrets renders a table of secrets
func (t *ResourceTable) RenderSecrets(secrets []*types.Secret) error {
	if len(secrets) == 0 {
		fmt.Println("No secrets found")
		return nil
	}

	// Set default headers if not provided
	if len(t.Headers) == 0 {
		t.Headers = []string{"NAME", "NAMESPACE", "TYPE", "VERSION", "AGE"}
	}

	// Create rows
	rows := [][]string{t.Headers} // Start with headers

	// Generate data rows
	for _, secret := range secrets {
		age := formatAgeTable(secret.CreatedAt)
		row := []string{
			secret.Name,
			secret.Namespace,
			secret.Type,
			fmt.Sprintf("%d", secret.Version),
			age,
		}
		rows = append(rows, row)
	}

	// Render the table with pterm
	return t.tableRenderer.WithData(rows).Render()
}

// RenderConfigmaps renders a table of configmaps
func (t *ResourceTable) RenderConfigmaps(configmaps []*types.Configmap) error {
	if len(configmaps) == 0 {
		fmt.Println("No configmaps found")
		return nil
	}

	// Set default headers if not provided
	if len(t.Headers) == 0 {
		t.Headers = []string{"NAME", "NAMESPACE", "VERSION", "AGE"}
	}

	// Create rows
	rows := [][]string{t.Headers} // Start with headers

	// Generate data rows
	for _, configmap := range configmaps {
		age := formatAgeTable(configmap.CreatedAt)
		row := []string{
			configmap.Name,
			configmap.Namespace,
			fmt.Sprintf("%d", configmap.Version),
			age,
		}
		rows = append(rows, row)
	}

	// Render the table with pterm
	return t.tableRenderer.WithData(rows).Render()
}

// Helper functions with unique names to avoid conflicts

// formatAgeTable formats a time.Time as a human-readable age string
func formatAgeTable(t time.Time) string {
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

// stripAnsiTable removes ANSI color codes from a string for accurate length calculation
func stripAnsiTable(s string) string {
	// Simple regex to strip ANSI color codes
	ansiRegex := regexp.MustCompile("\x1b\\[[0-9;]*m")
	return ansiRegex.ReplaceAllString(s, "")
}
