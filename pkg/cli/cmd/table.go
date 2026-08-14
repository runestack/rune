package cmd

import (
	"fmt"
	"regexp"
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
	// Namespace is the resolved namespace scope used when rendering an
	// empty-results message (e.g. "No services found in namespace 'stg'").
	// Ignored when AllNamespaces is true.
	Namespace string

	// Rendering details
	tableRenderer *pterm.TablePrinter
	stripAnsiFunc func(string) string
}

// emptyMessage returns a human-readable "no results" message for the given
// pluralised resource label, scoped by the table's Namespace / AllNamespaces.
func (t *ResourceTable) emptyMessage(resourcePlural string) string {
	switch {
	case t.AllNamespaces:
		return fmt.Sprintf("No %s found in any namespace", resourcePlural)
	case t.Namespace != "":
		return fmt.Sprintf("No %s found in namespace '%s'", resourcePlural, t.Namespace)
	default:
		return fmt.Sprintf("No %s found", resourcePlural)
	}
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
		fmt.Println(t.emptyMessage("services"))
		return nil
	}

	// Set default headers if not provided
	if len(t.Headers) == 0 {
		if t.AllNamespaces {
			t.Headers = []string{"NAMESPACE", "NAME", "TYPE", "STATUS", "READY", "REASON", "EXTERNAL", "GEN", "AGE"}
		} else {
			t.Headers = []string{"NAME", "TYPE", "STATUS", "READY", "REASON", "EXTERNAL", "GEN", "AGE"}
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

		// Format ready count.
		//
		// Prefer the truth from service.Instances when the server has
		// inlined them (rune get service[s] both do this now). Counting
		// Status==Running gives operators the real ready count instead
		// of inferring scale from service.Status — important because
		// service.Status==Running on a service with a readiness probe
		// only flips to Running after the probe passes, but a service
		// with NO readiness probe also reports Running and the old
		// inference was equivalent. The fallback below preserves the
		// old behaviour for paths that don't inline instances.
		running := 0
		if len(service.Instances) > 0 {
			for i := range service.Instances {
				if service.Instances[i].Status == types.InstanceStatusRunning {
					running++
				}
			}
		} else if service.Status == types.ServiceStatusRunning {
			running = service.Scale
		}
		instances := fmt.Sprintf("%d/%d", running, service.Scale)

		// Reason: short slug. Empty when service is healthy.
		reason := service.StatusReason
		if reason == "" {
			reason = "-"
		}

		// External endpoint (best-effort).
		// With the ingress controller, the canonical external URL is
		// https://{Expose.Host}{Expose.Path}. We don't know the scheme
		// for sure (could be http if no TLS), so default to https when
		// TLS is configured and http otherwise.
		external := "-"
		if service.Expose != nil && service.Expose.Host != "" {
			scheme := "http"
			if service.Expose.TLS != nil {
				scheme = "https"
			}
			external = fmt.Sprintf("%s://%s%s", scheme, service.Expose.Host, service.Expose.Path)
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
				reason,
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
				reason,
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

// shortInstanceID abbreviates a UUID to its first segment (8 hex chars),
// matching how the Docker runner names containers (<ns>-<name>-<id8>). The
// full UUID is noise in the table — instance NAMES are unique among live
// instances, and the short id still disambiguates a Failed tombstone from its
// live replacement. `rune logs`/`get` accept this prefix (unique prefixes
// resolve like git/docker short ids); the full id remains in -o json/yaml.
func shortInstanceID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// RenderInstances renders a table of instances
func (t *ResourceTable) RenderInstances(instances []*types.Instance) error {
	if len(instances) == 0 {
		fmt.Println(t.emptyMessage("instances"))
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
				shortInstanceID(instance.ID),
				instance.ServiceName,
				instance.NodeID,
				status,
				restarts,
				age,
			}
		} else {
			row = []string{
				instance.Name,
				shortInstanceID(instance.ID),
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
		// Namespaces are cluster-scoped; ignore Namespace/AllNamespaces here.
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
		fmt.Println(t.emptyMessage("deletion operations"))
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
		fmt.Println(t.emptyMessage("secrets"))
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
		fmt.Println(t.emptyMessage("configmaps"))
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

// RenderVolumes renders a table of volumes. Volumes previously rendered
// through a hand-rolled text/tabwriter, which produced an uncoloured table
// with different separators from every other `rune get` — including no status
// highlighting, so a Failed or Stalled volume looked identical to a healthy
// one.
func (t *ResourceTable) RenderVolumes(volumes []*types.Volume) error {
	if len(volumes) == 0 {
		fmt.Println(t.emptyMessage("volumes"))
		return nil
	}

	// Set default headers if not provided
	if len(t.Headers) == 0 {
		if t.AllNamespaces {
			t.Headers = []string{"NAMESPACE", "NAME", "STATUS", "CLASS", "SIZE", "ACCESS", "BOUND", "AGE"}
		} else {
			t.Headers = []string{"NAME", "STATUS", "CLASS", "SIZE", "ACCESS", "BOUND", "AGE"}
		}
	}

	// Create rows
	rows := [][]string{t.Headers} // Start with headers

	// Generate data rows
	for _, volume := range volumes {
		bound := "-"
		if volume.BoundClaim != "" {
			bound = volume.BoundClaim
		}
		access := string(volume.AccessMode)
		if access == "" {
			access = "-"
		}
		size := volume.Size
		if size == "" {
			size = "-"
		}
		status := format.PTermStatusLabel(string(volume.Status))
		age := formatAgeTable(volume.CreatedAt)

		var row []string
		if t.AllNamespaces {
			row = []string{
				volume.Namespace,
				volume.Name,
				status,
				volume.StorageClassName,
				size,
				access,
				bound,
				age,
			}
		} else {
			row = []string{
				volume.Name,
				status,
				volume.StorageClassName,
				size,
				access,
				bound,
				age,
			}
		}
		rows = append(rows, row)
	}

	// Render the table with pterm
	return t.tableRenderer.WithData(rows).Render()
}

// RenderEvents renders a table of resource events, newest-first as
// supplied by the caller. The TARGET column is "<kind>/<name>" with the
// kind lowercased (e.g. instance/gateway-0) to match the resource-ref
// style used elsewhere in the CLI and in `rune describe`.
func (t *ResourceTable) RenderEvents(events []*generated.Event) error {
	if len(events) == 0 {
		fmt.Println(t.emptyMessage("events"))
		return nil
	}

	if len(t.Headers) == 0 {
		t.Headers = []string{"TIME", "LEVEL", "TARGET", "COUNT", "REASON", "MESSAGE"}
	}

	rows := [][]string{t.Headers}
	for _, e := range events {
		count := ""
		if e.Count > 1 {
			count = fmt.Sprintf("×%d", e.Count)
		}
		target := strings.ToLower(e.Kind) + "/" + e.Name
		rows = append(rows, []string{
			e.LastSeen,
			format.PTermEventLevelLabel(e.Level),
			target,
			count,
			e.Reason,
			e.Message,
		})
	}

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
