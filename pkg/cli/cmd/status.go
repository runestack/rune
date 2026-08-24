package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pterm/pterm"
	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

// statusTable returns a pterm table configured the same way as the rest of
// the CLI: bold cyan headers, ANSI-width-aware column sizing. Use this
// instead of tabwriter when cells contain colored content — tabwriter counts
// escape codes as visible width, which is what caused the misaligned `-A`
// output before the refactor.
func statusTable() *pterm.TablePrinter {
	t := pterm.DefaultTable.WithHasHeader(true)
	return t.WithHeaderStyle(pterm.NewStyle(pterm.FgCyan, pterm.Bold))
}

// statusOptions holds the options for the `rune status` subcommand.
type statusOptions struct {
	cmdOptions

	allNamespaces bool
	watch         bool
	watchInterval time.Duration
	outputFormat  string        // "" (text), "json", "yaml"
	detail        bool          // expand each namespace into per-service rows
	noRollUp      bool          // skip the roll-up header (useful when piping text)
	since         time.Duration // recent-activity window (P3)

	// global is the resolved scope: true => roll up every namespace (the
	// default for a bare `rune status`); false => focus the single namespace
	// the user named with -n. Computed in runStatus from whether -n was
	// passed (and -A, which forces global). See the Scope model in
	// the RUNE-0XY status-command design.
	global bool
}

// newStatusCmd creates the status command. The default output is a
// human-readable namespace summary with a health roll-up at the top and a
// per-service table below. With -A, summarises every namespace; with -w,
// re-renders every interval; with -o json/yaml, emits structured data.
func newStatusCmd() *cobra.Command {
	opts := &statusOptions{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show a health summary for the whole cluster (or a namespace)",
		Long: `Show an at-a-glance health summary.

By default — with no -n — this reports on the WHOLE cluster: a one-line
roll-up for every namespace. Name a namespace with -n to focus on it and
get the full per-service table (status, ready/desired scale, image, age,
and the reason/message for anything not Running).

Examples:
  rune status                         # GLOBAL: roll-up across all namespaces
  rune status -n prod                 # focus one namespace (per-service table)
  rune status --detail                # per-service table for every namespace
  rune status -w                      # re-render every 2s (Ctrl+C to exit)
  rune status -o json                 # structured output for scripting`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Global by default. -n focuses a namespace; -A forces global
			// even if a default namespace is configured (back-compat alias).
			opts.global = opts.allNamespaces || !cmd.Flags().Changed("namespace")
			return runStatus(cmd, args, opts)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Focus a single namespace (default: roll up all namespaces)")
	cmd.Flags().BoolVarP(&opts.allNamespaces, "all-namespaces", "A", false, "Roll up every namespace (the default; kept for back-compat)")
	cmd.Flags().BoolVarP(&opts.watch, "watch", "w", false, "Re-render every --watch-interval seconds until Ctrl+C")
	cmd.Flags().DurationVar(&opts.watchInterval, "watch-interval", 2*time.Second, "Refresh interval for --watch")
	cmd.Flags().StringVarP(&opts.outputFormat, "output", "o", "", "Output format: '' (text), 'json', 'yaml'")
	cmd.Flags().BoolVar(&opts.detail, "detail", false, "In the global view, include the per-service table for each namespace")
	cmd.Flags().BoolVar(&opts.noRollUp, "no-roll-up", false, "Hide the roll-up header (useful when piping)")
	cmd.Flags().DurationVar(&opts.since, "since", 15*time.Minute, "Recent-activity window: show WARN/ERR events from the last N (0 disables the feed)")
	cmd.Flags().StringVar(&opts.addressOverride, "api-server", "", "Address of the API server")

	return cmd
}

func init() { rootCmd.AddCommand(newStatusCmd()) }

// runStatus is the entrypoint: collects the data once (or on a watch loop)
// and dispatches to the requested renderer.
func runStatus(cmd *cobra.Command, args []string, opts *statusOptions) error {
	api, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer api.Close()

	render := func() error {
		report, err := collectStatus(api, opts)
		if err != nil {
			return err
		}
		return renderStatus(os.Stdout, report, opts)
	}

	if !opts.watch {
		return render()
	}
	return watchStatus(render, opts.watchInterval)
}

// statusReport is the structured form the data is collected into. The same
// shape is shared by the human renderer and JSON/YAML output, so anything a
// user can see in the table is also queryable from a script.
type statusReport struct {
	// Server / Context identify which cluster this summary is about. Read
	// from the active CLI context (no extra RPC). Empty when unconfigured.
	Server  string `json:"server,omitempty" yaml:"server,omitempty"`
	Context string `json:"context,omitempty" yaml:"context,omitempty"`
	// Cluster is the global cluster section (P2). Collected only for the
	// global view; nil when focused (-n) or when unavailable. Each field
	// is independently best-effort — admin-only RPCs (network/registries)
	// stay nil for non-admin callers rather than failing the command.
	Cluster *clusterReport `json:"cluster,omitempty" yaml:"cluster,omitempty"`
	// RecentActivity is the WARN/ERR event feed (P3) within --since. Spans
	// all namespaces in the global view; one namespace under -n. Each brief
	// carries its namespace so the global renderer can prefix it.
	RecentActivity []activityBrief   `json:"recentActivity,omitempty" yaml:"recentActivity,omitempty"`
	Namespaces     []namespaceReport `json:"namespaces" yaml:"namespaces"`
}

// clusterReport is the global cluster section. Sourced from real RPCs:
// server version (GetServerVersion), runner readiness + store health
// (GetHealth), networking (admin NetworkStatus), and registry auth (admin
// RegistriesStatus). Each field is best-effort and independently omitted
// when its probe is unavailable/denied.
type clusterReport struct {
	ServerVersion string            `json:"serverVersion,omitempty" yaml:"serverVersion,omitempty"`
	Runners       map[string]string `json:"runners,omitempty" yaml:"runners,omitempty"`
	Store         string            `json:"store,omitempty" yaml:"store,omitempty"`
	NodeUsage     *nodeUsageBrief   `json:"nodeUsage,omitempty" yaml:"nodeUsage,omitempty"`
	Network       *networkBrief     `json:"network,omitempty" yaml:"network,omitempty"`
	Registries    *registriesBrief  `json:"registries,omitempty" yaml:"registries,omitempty"`

	// GPUs is device-probe health, one entry per node that has something
	// to say. NIL — not an empty map — on a machine with no GPUs and a
	// clean probe, so renderCluster emits no line at all rather than
	// "GPUs: none".
	GPUs map[string]string `json:"gpus,omitempty" yaml:"gpus,omitempty"`
}

// nodeUsageBrief is live node pressure: CPU as a percent of capacity
// (CPUUsedPercent < 0 means usage couldn't be sampled) and memory used vs
// total bytes. Single-node today, so this is effectively this host.
type nodeUsageBrief struct {
	CPUCores       float64 `json:"cpuCores" yaml:"cpuCores"`
	CPUUsedPercent float64 `json:"cpuUsedPercent" yaml:"cpuUsedPercent"`
	MemUsedBytes   int64   `json:"memUsedBytes" yaml:"memUsedBytes"`
	MemTotalBytes  int64   `json:"memTotalBytes" yaml:"memTotalBytes"`
}

type networkBrief struct {
	CIDR          string `json:"cidr" yaml:"cidr"`
	VIPsAllocated int    `json:"vipsAllocated" yaml:"vipsAllocated"`
	Capacity      int    `json:"capacity" yaml:"capacity"`
}

type registriesBrief struct {
	Total   int `json:"total" yaml:"total"`
	OK      int `json:"ok" yaml:"ok"`
	Failing int `json:"failing" yaml:"failing"`
}

// activityBrief is a projection of a WARN/ERR Event for the recent-activity
// feed. LastSeen is humanized (e.g. "5m") at collection time.
type activityBrief struct {
	Namespace string `json:"namespace" yaml:"namespace"`
	Kind      string `json:"kind" yaml:"kind"`
	Name      string `json:"name" yaml:"name"`
	Level     string `json:"level" yaml:"level"`
	Reason    string `json:"reason,omitempty" yaml:"reason,omitempty"`
	Message   string `json:"message,omitempty" yaml:"message,omitempty"`
	Count     int    `json:"count" yaml:"count"`
	LastSeen  string `json:"lastSeen,omitempty" yaml:"lastSeen,omitempty"`
}

type namespaceReport struct {
	Namespace string          `json:"namespace" yaml:"namespace"`
	Summary   statusSummary   `json:"summary" yaml:"summary"`
	Services  []serviceReport `json:"services" yaml:"services"`
}

// statusSummary counts services bucketed by status. The same bucket names are
// used in the human roll-up and the JSON keys so they don't drift.
type statusSummary struct {
	Total     int `json:"total" yaml:"total"`
	Running   int `json:"running" yaml:"running"`
	Deploying int `json:"deploying" yaml:"deploying"`
	Stopping  int `json:"stopping" yaml:"stopping"`
	Pending   int `json:"pending" yaml:"pending"`
	Failed    int `json:"failed" yaml:"failed"`
	// Instances is the total count of non-deleted instances in the namespace.
	Instances int `json:"instances" yaml:"instances"`
	// InstanceStates breaks the total down by instance status. Added
	// alongside Instances (kept for back-compat) rather than replacing it.
	InstanceStates instanceStateCounts `json:"instanceStates" yaml:"instanceStates"`
	// Secrets / Configmaps counts (P2). Best-effort; 0 if the list failed.
	Secrets    int `json:"secrets" yaml:"secrets"`
	Configmaps int `json:"configmaps" yaml:"configmaps"`
}

// instanceStateCounts buckets non-deleted instances by status for the
// summary. Starting folds in Pending/Created (all "coming up" states).
type instanceStateCounts struct {
	Running     int `json:"running" yaml:"running"`
	Starting    int `json:"starting" yaml:"starting"`
	Failed      int `json:"failed" yaml:"failed"`
	Stalled     int `json:"stalled" yaml:"stalled"`
	Terminating int `json:"terminating" yaml:"terminating"`
}

type serviceReport struct {
	Name           string `json:"name" yaml:"name"`
	Status         string `json:"status" yaml:"status"`
	Image          string `json:"image,omitempty" yaml:"image,omitempty"`
	DesiredScale   int    `json:"desiredScale" yaml:"desiredScale"`
	ReadyInstances int    `json:"readyInstances" yaml:"readyInstances"`
	Age            string `json:"age" yaml:"age"`
	StatusReason   string `json:"statusReason,omitempty" yaml:"statusReason,omitempty"`
	StatusMessage  string `json:"statusMessage,omitempty" yaml:"statusMessage,omitempty"`
	// Update is the in-flight rolling update, nil when none is running
	// (RUNE-042). Machine output carries the whole block; the human table
	// renders a single UPDATE cell from it.
	Update *types.UpdateStatus `json:"update,omitempty" yaml:"update,omitempty"`
	// updatedAt is kept for stable sorting and machine output; the human
	// table shows the derived Age string instead.
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
}

// collectStatus pulls services + instances from the API for the requested
// namespaces and assembles the report. One ListServices + one ListInstances
// per namespace — no N+1.
func collectStatus(api *client.Client, opts *statusOptions) (*statusReport, error) {
	sc := client.NewServiceClient(api)
	ic := client.NewInstanceClient(api)
	sec := client.NewSecretClient(api)
	cm := client.NewConfigmapClient(api)

	var namespaces []string
	if opts.global {
		nsClient := client.NewNamespaceClient(api)
		nss, err := nsClient.ListNamespaces(nil, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %w", err)
		}
		for _, ns := range nss {
			namespaces = append(namespaces, ns.Name)
		}
		sort.Strings(namespaces)
	} else {
		namespaces = []string{opts.namespace}
	}

	report := &statusReport{}
	report.Server, report.Context = activeContext()
	if opts.addressOverride != "" {
		report.Server = opts.addressOverride
	}
	for _, ns := range namespaces {
		nr, err := collectNamespace(sc, ic, sec, cm, ns)
		if err != nil {
			// Best-effort: include the error in the report rather than
			// failing the whole command — `-A` should still show
			// healthy namespaces even if one is broken.
			report.Namespaces = append(report.Namespaces, namespaceReport{
				Namespace: ns,
				Summary:   statusSummary{},
				Services:  []serviceReport{{Name: "(error)", Status: "Error", StatusMessage: err.Error()}},
			})
			continue
		}
		report.Namespaces = append(report.Namespaces, *nr)
	}

	// P2: cluster section is global-only (the focused -n view omits it).
	if opts.global {
		report.Cluster = collectCluster(api)
	}
	// P3: recent activity feed (best-effort).
	report.RecentActivity = collectRecentActivity(api, opts)

	return report, nil
}

// collectCluster assembles the global cluster section. Every probe is
// independent and best-effort: a nil sub-field (e.g. non-admin caller
// denied NetworkStatus) degrades that line, never the command. Returns
// nil only if nothing at all could be gathered.
func collectCluster(api *client.Client) *clusterReport {
	cr := &clusterReport{}

	hc := generated.NewHealthServiceClient(api.Conn())
	if ctx, cancel := api.Context(); true {
		if v, err := hc.GetServerVersion(ctx, &generated.GetServerVersionRequest{}); err == nil && v != nil {
			cr.ServerVersion = v.Version
		}
		cancel()
	}
	// Runner readiness (docker daemon ping etc.) — the high-value signal:
	// server up + store fine but Docker down means nothing can deploy.
	if ctx, cancel := api.Context(); true {
		if resp, err := hc.GetHealth(ctx, &generated.GetHealthRequest{ComponentType: "runner"}); err == nil && resp != nil && len(resp.Components) > 0 {
			m := map[string]string{}
			for _, c := range resp.Components {
				m[c.Name] = healthWord(c.Status, c.Message)
			}
			cr.Runners = m
		}
		cancel()
	}
	if ctx, cancel := api.Context(); true {
		if resp, err := hc.GetHealth(ctx, &generated.GetHealthRequest{ComponentType: "store"}); err == nil && resp != nil && len(resp.Components) > 0 {
			cr.Store = healthWord(resp.Components[0].Status, resp.Components[0].Message)
		}
		cancel()
	}
	// Device-probe health. Best-effort like every other probe here, and
	// deliberately silent when the server reports no components: an
	// absent signal must stay an absent line. An older server rejects the
	// component type, which lands in the same place.
	if ctx, cancel := api.Context(); true {
		if resp, err := hc.GetHealth(ctx, &generated.GetHealthRequest{ComponentType: "gpu"}); err == nil && resp != nil && len(resp.Components) > 0 {
			m := map[string]string{}
			for _, c := range resp.Components {
				m[c.Name] = healthWord(c.Status, c.Message)
			}
			cr.GPUs = m
		}
		cancel()
	}
	// Live node pressure (CPU%/mem). Single-node today, so "node" == host.
	if ctx, cancel := api.Context(); true {
		if resp, err := hc.GetHealth(ctx, &generated.GetHealthRequest{ComponentType: "node"}); err == nil && resp != nil && len(resp.Components) > 0 {
			if r := resp.Components[0].Resources; r != nil && (r.MemTotalBytes > 0 || r.CpuCores > 0) {
				cr.NodeUsage = &nodeUsageBrief{
					CPUCores:       r.CpuCores,
					CPUUsedPercent: r.CpuUsedPercent,
					MemUsedBytes:   r.MemUsedBytes,
					MemTotalBytes:  r.MemTotalBytes,
				}
			}
		}
		cancel()
	}

	ac := generated.NewAdminServiceClient(api.Conn())
	if ctx, cancel := api.Context(); true {
		if ns, err := ac.NetworkStatus(ctx, &generated.NetworkStatusRequest{}); err == nil && ns != nil && ns.Cidr != "" {
			cr.Network = &networkBrief{
				CIDR:          ns.Cidr,
				VIPsAllocated: len(ns.Allocations),
				Capacity:      int(ns.Capacity),
			}
		}
		cancel()
	}
	if ctx, cancel := api.Context(); true {
		if rs, err := ac.RegistriesStatus(ctx, &generated.RegistriesStatusRequest{}); err == nil && rs != nil {
			rb := &registriesBrief{Total: len(rs.Registries)}
			for _, r := range rs.Registries {
				if r.LastError == "" {
					rb.OK++
				} else {
					rb.Failing++
				}
			}
			cr.Registries = rb
		}
		cancel()
	}

	if cr.ServerVersion == "" && len(cr.Runners) == 0 && cr.Store == "" && cr.NodeUsage == nil && cr.Network == nil && cr.Registries == nil {
		return nil
	}
	return cr
}

// formatNodeUsage renders the live node pressure line, e.g.
// "CPU 38% (8 cores) · Mem 6.2 GiB / 16.0 GiB". CPU is omitted when usage
// couldn't be sampled (CPUUsedPercent < 0); memory is omitted when total is
// unknown — so a partial probe still shows whatever it has.
func formatNodeUsage(u *nodeUsageBrief) string {
	var parts []string
	if u.CPUUsedPercent >= 0 {
		cpu := fmt.Sprintf("CPU %.0f%%", u.CPUUsedPercent)
		if u.CPUCores > 0 {
			cpu += fmt.Sprintf(" (%g cores)", u.CPUCores)
		}
		parts = append(parts, cpu)
	} else if u.CPUCores > 0 {
		parts = append(parts, fmt.Sprintf("CPU %g cores", u.CPUCores))
	}
	if u.MemTotalBytes > 0 {
		parts = append(parts, fmt.Sprintf("Mem %s / %s", formatBytes(u.MemUsedBytes), formatBytes(u.MemTotalBytes)))
	} else if u.MemUsedBytes > 0 {
		parts = append(parts, fmt.Sprintf("Mem %s", formatBytes(u.MemUsedBytes)))
	}
	if len(parts) == 0 {
		return "(usage unavailable)"
	}
	return strings.Join(parts, " · ")
}

// formatBytes renders a byte count as a binary-unit string ("6.2 GiB").
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), units[exp])
}

// healthWord maps a GetHealth component status into a short display word.
// HEALTHY → "ready"; otherwise the server's message (e.g. "unreachable:
// <docker error>") so the operator sees the actual reason, falling back to
// a generic word when the message is empty.
func healthWord(st generated.HealthStatus, message string) string {
	if st == generated.HealthStatus_HEALTH_STATUS_HEALTHY {
		if message != "" && message != "ok" {
			return message
		}
		return "ready"
	}
	if message != "" {
		return message
	}
	return "unhealthy"
}

// collectRecentActivity returns WARN/ERR events within opts.since, newest
// first, capped. Global (no -n) spans all namespaces; focused reads one.
// Best-effort: returns nil if the event service is unavailable or disabled.
func collectRecentActivity(api *client.Client, opts *statusOptions) []activityBrief {
	if opts.since <= 0 {
		return nil
	}
	const maxItems = 20
	ns := ""
	if !opts.global {
		ns = opts.namespace
	}
	evs, err := client.NewEventClient(api).ListEvents(ns, "", "", maxItems*4)
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-opts.since)
	var out []activityBrief
	for _, e := range evs {
		if e == nil {
			continue
		}
		lvl := strings.ToUpper(e.Level)
		if lvl != "WARN" && lvl != "ERR" && lvl != "ERROR" {
			continue
		}
		// LastSeen is RFC3339; skip events outside the window. Parse
		// failures fall through (better to show than silently drop).
		if ts, perr := time.Parse(time.RFC3339, e.LastSeen); perr == nil && ts.Before(cutoff) {
			continue
		}
		out = append(out, activityBrief{
			Namespace: e.Namespace,
			Kind:      strings.ToLower(e.Kind),
			Name:      e.Name,
			Level:     lvl,
			Reason:    e.Reason,
			Message:   e.Message,
			Count:     int(e.Count),
			LastSeen:  humanizeEventAge(e.LastSeen),
		})
		if len(out) >= maxItems {
			break
		}
	}
	return out
}

// humanizeEventAge renders an RFC3339 timestamp as a compact age ("5m").
// Falls back to the raw string when it can't be parsed.
func humanizeEventAge(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return formatAge(t)
}

// activeContext returns the configured server address and current context
// name for the status header. Best-effort and RPC-free: returns empty
// strings when no context is configured, so status still renders.
func activeContext() (server, contextName string) {
	cfg, err := loadContextConfig()
	if err != nil || cfg == nil {
		return "", ""
	}
	contextName = cfg.CurrentContext
	if c, ok := cfg.Contexts[contextName]; ok {
		server = c.Server
	}
	return server, contextName
}

func collectNamespace(sc *client.ServiceClient, ic *client.InstanceClient, sec *client.SecretClient, cm *client.ConfigmapClient, namespace string) (*namespaceReport, error) {
	services, err := sc.ListServices(types.NS(namespace), "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to list services in %s: %w", namespace, err)
	}

	// Bucket instance counts per service (for ready/desired) and by state
	// (for the summary breakdown).
	ready := map[string]int{}
	totalInstances := 0
	var states instanceStateCounts
	if insts, err := ic.ListInstances(namespace, "", "", ""); err == nil {
		for _, inst := range insts {
			if inst == nil || inst.Status == types.InstanceStatusDeleted {
				continue
			}
			totalInstances++
			switch inst.Status {
			case types.InstanceStatusRunning:
				states.Running++
				ready[inst.ServiceName]++
			case types.InstanceStatusStarting, types.InstanceStatusPending, types.InstanceStatusCreated:
				states.Starting++
			case types.InstanceStatusFailed:
				states.Failed++
			case types.InstanceStatusStalled:
				states.Stalled++
			case types.InstanceStatusTerminating:
				states.Terminating++
			}
		}
	}

	nr := &namespaceReport{Namespace: namespace}
	for _, s := range services {
		updatedAt := time.Time{}
		if s.Metadata != nil {
			updatedAt = s.Metadata.UpdatedAt
		}
		nr.Services = append(nr.Services, serviceReport{
			Name:           s.Name,
			Status:         string(s.Status),
			Image:          s.Image,
			DesiredScale:   s.Scale,
			ReadyInstances: ready[s.Name],
			Age:            formatAge(updatedAt),
			StatusReason:   s.StatusReason,
			StatusMessage:  s.StatusMessage,
			Update:         s.Update,
			UpdatedAt:      updatedAt,
		})
		nr.Summary.Total++
		nr.Summary.Running += boolToInt(s.Status == types.ServiceStatusRunning)
		nr.Summary.Deploying += boolToInt(s.Status == types.ServiceStatusDeploying)
		nr.Summary.Stopping += boolToInt(s.Status == types.ServiceStatusStopping)
		nr.Summary.Pending += boolToInt(s.Status == types.ServiceStatusPending)
		nr.Summary.Failed += boolToInt(s.Status == types.ServiceStatusFailed)
	}
	nr.Summary.Instances = totalInstances
	nr.Summary.InstanceStates = states

	// Secrets / configmaps counts (best-effort; a list failure leaves 0).
	if secs, err := sec.ListSecrets(namespace, "", ""); err == nil {
		nr.Summary.Secrets = len(secs)
	}
	if cms, err := cm.ListConfigmaps(namespace, "", ""); err == nil {
		nr.Summary.Configmaps = len(cms)
	}

	// Stable sort: needs-attention statuses bubble up, ties broken by name.
	sort.SliceStable(nr.Services, func(i, j int) bool {
		if pi, pj := statusPriority(nr.Services[i].Status), statusPriority(nr.Services[j].Status); pi != pj {
			return pi < pj
		}
		return nr.Services[i].Name < nr.Services[j].Name
	})
	return nr, nil
}

// statusPriority orders rows in the per-service table so what needs operator
// attention floats to the top. Lower = higher priority.
func statusPriority(status string) int {
	switch types.ServiceStatus(status) {
	case types.ServiceStatusFailed:
		return 0
	case types.ServiceStatusStopping:
		return 1
	case types.ServiceStatusDeploying:
		return 2
	case types.ServiceStatusPending:
		return 3
	case types.ServiceStatusRunning:
		return 4
	default:
		return 5
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// renderStatus dispatches to the requested output format.
func renderStatus(w *os.File, report *statusReport, opts *statusOptions) error {
	switch strings.ToLower(opts.outputFormat) {
	case "json":
		return outputJSON(report)
	case "yaml":
		return outputYAML(report)
	case "", "text", "table":
		return renderStatusText(w, report, opts)
	default:
		return fmt.Errorf("unknown output format: %s (want '', 'json', or 'yaml')", opts.outputFormat)
	}
}

// renderStatusText is the human-readable renderer. Global (the default)
// prints the cluster/context header + a per-namespace roll-up; focused
// (-n) prints one namespace with its full per-service table.
func renderStatusText(w *os.File, report *statusReport, opts *statusOptions) error {
	renderStatusBanner(w, report, opts.noRollUp)
	if opts.global {
		renderCluster(w, report.Cluster)
		if err := renderAllNamespaces(w, report, opts); err != nil {
			return err
		}
		renderRecentActivity(w, report.RecentActivity, true)
		return nil
	}
	if len(report.Namespaces) == 0 {
		fmt.Fprintln(w, "No namespaces found")
		return nil
	}
	nr := report.Namespaces[0]
	renderHeader(w, nr, opts.noRollUp)
	renderServiceTable(w, nr.Services)
	renderRecentActivity(w, report.RecentActivity, false)
	return nil
}

// renderCluster prints the global cluster section. Lines are emitted only
// for signals that were actually gathered, so a non-admin caller (no
// network/registries access) just sees the server line. nil => no section.
func renderCluster(w *os.File, c *clusterReport) {
	if c == nil {
		return
	}
	fmt.Fprintln(w, format.Highlight("Cluster"))
	if c.ServerVersion != "" {
		fmt.Fprintf(w, "  %-11s %s\n", "Server:", c.ServerVersion)
	}
	if len(c.Runners) > 0 {
		// Stable order so --watch output doesn't jitter.
		names := make([]string, 0, len(c.Runners))
		for n := range c.Runners {
			names = append(names, n)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, fmt.Sprintf("%s=%s", n, c.Runners[n]))
		}
		fmt.Fprintf(w, "  %-11s %s\n", "Runners:", strings.Join(parts, ", "))
	}
	if c.Store != "" {
		fmt.Fprintf(w, "  %-11s %s\n", "Store:", c.Store)
	}
	if c.NodeUsage != nil {
		fmt.Fprintf(w, "  %-11s %s\n", "Node:", formatNodeUsage(c.NodeUsage))
	}
	if len(c.GPUs) > 0 {
		names := make([]string, 0, len(c.GPUs))
		for n := range c.GPUs {
			names = append(names, n)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, n := range names {
			// Single-node has one node and naming it adds nothing; on
			// multi-node the name is the whole point.
			if len(names) == 1 {
				parts = append(parts, c.GPUs[n])
			} else {
				parts = append(parts, fmt.Sprintf("%s=%s", n, c.GPUs[n]))
			}
		}
		fmt.Fprintf(w, "  %-11s %s\n", "GPUs:", strings.Join(parts, ", "))
	}
	if c.Network != nil {
		extra := ""
		if c.Network.Capacity > 0 {
			extra = fmt.Sprintf(", capacity %d", c.Network.Capacity)
		}
		fmt.Fprintf(w, "  %-11s cidr=%s, vips=%d%s\n", "Network:", c.Network.CIDR, c.Network.VIPsAllocated, extra)
	}
	if c.Registries != nil {
		r := c.Registries
		line := fmt.Sprintf("%d (ok %d", r.Total, r.OK)
		if r.Failing > 0 {
			line += fmt.Sprintf(", failing %d", r.Failing)
		}
		line += ")"
		fmt.Fprintf(w, "  %-11s %s\n", "Registries:", line)
	}
	fmt.Fprintln(w)
}

// renderRecentActivity prints the WARN/ERR feed. global=true prefixes each
// line with its namespace (the feed spans the cluster). Nothing prints when
// the feed is empty — a quiet cluster shouldn't grow a blank section.
func renderRecentActivity(w *os.File, items []activityBrief, global bool) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintln(w, format.Highlight("Recent activity"))
	for _, a := range items {
		target := fmt.Sprintf("%s/%s", a.Kind, a.Name)
		if global && a.Namespace != "" {
			target = fmt.Sprintf("%s/%s", a.Namespace, target)
		}
		line := fmt.Sprintf("  %-5s %s %s", a.LastSeen, format.PTermEventLevelLabel(a.Level), target)
		if a.Reason != "" {
			line += "  " + a.Reason
		}
		if a.Count > 1 {
			line += fmt.Sprintf("  ×%d", a.Count)
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)
}

// renderStatusBanner prints the "server / context" line that identifies
// which cluster this summary describes. Skipped when --no-roll-up (piping)
// or when no context is configured.
func renderStatusBanner(w *os.File, report *statusReport, hideRollUp bool) {
	if hideRollUp || (report.Server == "" && report.Context == "") {
		return
	}
	parts := []string{}
	if report.Server != "" {
		parts = append(parts, fmt.Sprintf("server=%s", report.Server))
	}
	if report.Context != "" {
		parts = append(parts, fmt.Sprintf("context=%s", report.Context))
	}
	fmt.Fprintf(w, "%s   %s\n\n", format.Highlight("Rune Status"), format.Dim("%s", strings.Join(parts, "   ")))
}

func renderAllNamespaces(w *os.File, report *statusReport, opts *statusOptions) error {
	totalSvc, totalInst := 0, 0
	for _, nr := range report.Namespaces {
		totalSvc += nr.Summary.Total
		totalInst += nr.Summary.Instances
	}
	if !opts.noRollUp {
		fmt.Fprintf(w, "%d namespaces · %d services · %d instances\n\n",
			len(report.Namespaces), totalSvc, totalInst)
	}

	if opts.detail {
		for i, nr := range report.Namespaces {
			if i > 0 {
				fmt.Fprintln(w)
			}
			renderHeader(w, nr, opts.noRollUp)
			renderServiceTable(w, nr.Services)
		}
		return nil
	}

	rows := [][]string{{"NAMESPACE", "SERVICES", "RUNNING", "DEPLOYING", "STOPPING", "FAILED", "PENDING", "INSTANCES"}}
	for _, nr := range report.Namespaces {
		s := nr.Summary
		rows = append(rows, []string{
			nr.Namespace,
			fmt.Sprintf("%d", s.Total),
			countCell(s.Running, types.ServiceStatusRunning),
			countCell(s.Deploying, types.ServiceStatusDeploying),
			countCell(s.Stopping, types.ServiceStatusStopping),
			countCell(s.Failed, types.ServiceStatusFailed),
			countCell(s.Pending, types.ServiceStatusPending),
			instancesCell(s),
		})
	}
	return statusTable().WithData(rows).Render()
}

// instancesCell summarizes a namespace's instances for the global roll-up:
// the total, plus any non-running states that warrant attention. A fully
// healthy namespace just shows the count (e.g. "21"); one with trouble
// shows "28 (1 starting, 2 failed)".
func instancesCell(s statusSummary) string {
	st := s.InstanceStates
	var extra []string
	if st.Starting > 0 {
		extra = append(extra, fmt.Sprintf("%d starting", st.Starting))
	}
	if st.Failed > 0 {
		extra = append(extra, fmt.Sprintf("%d failed", st.Failed))
	}
	if st.Stalled > 0 {
		extra = append(extra, fmt.Sprintf("%d stalled", st.Stalled))
	}
	if st.Terminating > 0 {
		extra = append(extra, fmt.Sprintf("%d terminating", st.Terminating))
	}
	if len(extra) == 0 {
		return fmt.Sprintf("%d", s.Instances)
	}
	return fmt.Sprintf("%d (%s)", s.Instances, strings.Join(extra, ", "))
}

// renderHeader prints the namespace header and the bucketed roll-up.
// Buckets with zero services are omitted to keep the eye on what's actually
// present — e.g. a healthy namespace just shows "✓ Running 12", not five
// rows of zeros.
func renderHeader(w *os.File, nr namespaceReport, hideRollUp bool) {
	fmt.Fprintf(w, "Namespace: %s   ·   %d services   ·   %d instances   ·   %d secrets   ·   %d configmaps\n",
		format.Highlight("%s", nr.Namespace), nr.Summary.Total, nr.Summary.Instances,
		nr.Summary.Secrets, nr.Summary.Configmaps)
	if hideRollUp {
		fmt.Fprintln(w)
		return
	}
	fmt.Fprintln(w)

	s := nr.Summary
	// Render in attention-priority order (matches the per-service sort).
	// Empty namespaces still get a single "Pending 0" line so the user
	// isn't left with just a blank header.
	rows := []struct {
		status types.ServiceStatus
		label  string
		count  int
	}{
		{types.ServiceStatusFailed, "Failed", s.Failed},
		{types.ServiceStatusStopping, "Stopping", s.Stopping},
		{types.ServiceStatusDeploying, "Deploying", s.Deploying},
		{types.ServiceStatusPending, "Pending", s.Pending},
		{types.ServiceStatusRunning, "Running", s.Running},
	}
	any := false
	for _, r := range rows {
		if r.count == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s %-10s %d\n", glyphFor(r.status), r.label, r.count)
		any = true
	}
	if !any {
		fmt.Fprintf(w, "  %s\n", format.Dim("no services"))
	}

	// Instance state breakdown — one line so operators see Starting/Failed/
	// Stalled without running `rune get instances`.
	st := s.InstanceStates
	fmt.Fprintf(w, "\n  %s Running %d · Starting %d · Failed %d · Stalled %d · Terminating %d\n",
		format.Dim("Instances:"), st.Running, st.Starting, st.Failed, st.Stalled, st.Terminating)
	fmt.Fprintln(w)
}

// renderServiceTable prints the per-service rows. Reason/Message only
// populated for non-Running rows so the column stays narrow when everything
// is healthy. Uses pterm so colored cells line up correctly (tabwriter
// counts ANSI escape bytes as visible width and misaligns).
func renderServiceTable(_ *os.File, services []serviceReport) {
	// The UPDATE column appears only when something in this list is actually
	// updating. A steady cluster reads exactly as it did before RUNE-042 —
	// a permanently-empty column is a permanent question ("should something
	// be there?") for a feature most services are not using at any moment.
	updating := anyUpdating(services)

	header := []string{"NAME", "STATUS", "SCALE", "IMAGE", "AGE", "REASON / MESSAGE"}
	if updating {
		header = []string{"NAME", "STATUS", "SCALE", "UPDATE", "IMAGE", "AGE", "REASON / MESSAGE"}
	}
	rows := [][]string{header}

	for _, s := range services {
		reason := ""
		if s.Status != string(types.ServiceStatusRunning) {
			switch {
			case s.StatusReason != "" && s.StatusMessage != "":
				reason = fmt.Sprintf("%s — %s", s.StatusReason, s.StatusMessage)
			case s.StatusReason != "":
				reason = s.StatusReason
			case s.StatusMessage != "":
				reason = s.StatusMessage
			}
		}
		row := []string{
			s.Name,
			colorStatus(s.Status),
			// ready/desired (k8s convention) — 2/3 = 2 ready of 3 desired.
			fmt.Sprintf("%d/%d", s.ReadyInstances, s.DesiredScale),
		}
		if updating {
			row = append(row, updateCell(s.Update))
		}
		row = append(row, shortImage(s.Image), s.Age, reason)
		rows = append(rows, row)
	}
	_ = statusTable().WithData(rows).Render()
}

// anyUpdating reports whether any service in the list has an update in flight.
func anyUpdating(services []serviceReport) bool {
	for _, s := range services {
		if s.Update != nil {
			return true
		}
	}
	return false
}

// updateCell renders the compact per-row form: how many replicas carry the new
// template, out of the desired count. The full sentence lives in
// `rune describe`; the table only has room for the fraction.
func updateCell(u *types.UpdateStatus) string {
	if u == nil {
		return "—"
	}
	return fmt.Sprintf("%d/%d replaced", u.UpdatedReady, u.Desired)
}

// shortImage trims a registry/repo prefix to keep the service table narrow,
// keeping the final path segment (image name + tag). "…/" marks truncation:
// ghcr.io/org/proj/api:dev -> …/api:dev. Empty stays "-".
func shortImage(image string) string {
	if image == "" {
		return "-"
	}
	if i := strings.LastIndex(image, "/"); i >= 0 {
		return "…/" + image[i+1:]
	}
	return image
}

// glyphFor returns a bucket glyph for the roll-up. Auto-degrades to ASCII
// when colors are off (so CI logs / piped output stay readable).
func glyphFor(status types.ServiceStatus) string {
	useUnicode := format.IsColorEnabled()
	switch status {
	case types.ServiceStatusRunning:
		if useUnicode {
			return format.Success("✓")
		}
		return "OK   "
	case types.ServiceStatusDeploying:
		if useUnicode {
			return format.Info("⊙")
		}
		return "DEPL "
	case types.ServiceStatusStopping:
		if useUnicode {
			return format.Warning("⏸")
		}
		return "STOP "
	case types.ServiceStatusFailed:
		if useUnicode {
			return format.Error("⚠")
		}
		return "FAIL "
	case types.ServiceStatusPending:
		if useUnicode {
			return format.Dim("·")
		}
		return "PEND "
	default:
		return "?"
	}
}

// colorStatus colors the status word in the table to match the roll-up.
func colorStatus(status string) string {
	switch types.ServiceStatus(status) {
	case types.ServiceStatusRunning:
		return format.Success("%s", status)
	case types.ServiceStatusFailed:
		return format.Error("%s", status)
	case types.ServiceStatusStopping:
		return format.Warning("%s", status)
	case types.ServiceStatusDeploying:
		return format.Info("%s", status)
	default:
		return status
	}
}

// countCell renders a roll-up count in the -A table, dimming the cell when
// the count is zero so the eye skips it.
func countCell(n int, status types.ServiceStatus) string {
	if n == 0 {
		return format.Dim("0")
	}
	switch status {
	case types.ServiceStatusFailed:
		return format.Error("%d", n)
	case types.ServiceStatusStopping:
		return format.Warning("%d", n)
	case types.ServiceStatusDeploying:
		return format.Info("%d", n)
	case types.ServiceStatusRunning:
		return format.Success("%d", n)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// watchStatus re-renders on a tick until the user hits Ctrl+C. Clears the
// screen between frames so the table redraws in place — feels like top(1)
// rather than an ever-growing scrollback.
func watchStatus(render func() error, interval time.Duration) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)

	go func() {
		<-sig
		cancel()
	}()

	for {
		// ANSI clear-screen + cursor-home. Skipped automatically when stdout
		// isn't a TTY (clear codes look fine in scrollback anyway, but the
		// watch loop is mainly an interactive thing).
		fmt.Print("\033[2J\033[H")
		if err := render(); err != nil {
			return err
		}
		fmt.Printf("\nrefreshing every %s · Ctrl+C to exit\n", interval)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
