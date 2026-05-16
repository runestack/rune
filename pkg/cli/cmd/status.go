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
	outputFormat  string // "" (text), "json", "yaml"
	detail        bool   // with -A, expand each namespace into per-service rows
	noRollUp      bool   // skip the roll-up header (useful when piping text)
}

// newStatusCmd creates the status command. The default output is a
// human-readable namespace summary with a health roll-up at the top and a
// per-service table below. With -A, summarises every namespace; with -w,
// re-renders every interval; with -o json/yaml, emits structured data.
func newStatusCmd() *cobra.Command {
	opts := &statusOptions{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show a quick health summary for services",
		Long: `Show a roll-up of service health for a namespace (or all namespaces with -A).

The default output groups services into Running / Deploying / Stopping /
Failed / Pending and prints a per-service table with the desired/ready
scale, age, and — for non-Running rows — the status reason and message
so you don't need a second command to learn why something is unhealthy.

Examples:
  rune status                         # default namespace
  rune status -n prod                 # specific namespace
  rune status -A                      # all namespaces, one summary line each
  rune status -A --detail             # all namespaces + per-service table for each
  rune status -w                      # re-render every 2s (Ctrl+C to exit)
  rune status -o json                 # structured output for scripting`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !opts.allNamespaces {
				opts.namespace = effectiveCmdNS(opts.namespace)
			}
			return runStatus(cmd, args, opts)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace to summarize (ignored with -A)")
	cmd.Flags().BoolVarP(&opts.allNamespaces, "all-namespaces", "A", false, "Summarize every namespace")
	cmd.Flags().BoolVarP(&opts.watch, "watch", "w", false, "Re-render every --watch-interval seconds until Ctrl+C")
	cmd.Flags().DurationVar(&opts.watchInterval, "watch-interval", 2*time.Second, "Refresh interval for --watch")
	cmd.Flags().StringVarP(&opts.outputFormat, "output", "o", "", "Output format: '' (text), 'json', 'yaml'")
	cmd.Flags().BoolVar(&opts.detail, "detail", false, "With -A, include the per-service table for each namespace")
	cmd.Flags().BoolVar(&opts.noRollUp, "no-roll-up", false, "Hide the roll-up header (useful when piping)")
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
	Namespaces []namespaceReport `json:"namespaces" yaml:"namespaces"`
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
}

type serviceReport struct {
	Name           string `json:"name" yaml:"name"`
	Status         string `json:"status" yaml:"status"`
	DesiredScale   int    `json:"desiredScale" yaml:"desiredScale"`
	ReadyInstances int    `json:"readyInstances" yaml:"readyInstances"`
	Age            string `json:"age" yaml:"age"`
	StatusReason   string `json:"statusReason,omitempty" yaml:"statusReason,omitempty"`
	StatusMessage  string `json:"statusMessage,omitempty" yaml:"statusMessage,omitempty"`
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

	var namespaces []string
	if opts.allNamespaces {
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
	for _, ns := range namespaces {
		nr, err := collectNamespace(sc, ic, ns)
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
	return report, nil
}

func collectNamespace(sc *client.ServiceClient, ic *client.InstanceClient, namespace string) (*namespaceReport, error) {
	services, err := sc.ListServices(types.NS(namespace), "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to list services in %s: %w", namespace, err)
	}

	// Bucket instance counts per service so we can render desired/ready.
	ready := map[string]int{}
	totalInstances := 0
	if insts, err := ic.ListInstances(namespace, "", "", ""); err == nil {
		for _, inst := range insts {
			if inst == nil || inst.Status == types.InstanceStatusDeleted {
				continue
			}
			totalInstances++
			if inst.Status == types.InstanceStatusRunning {
				ready[inst.ServiceName]++
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
			DesiredScale:   s.Scale,
			ReadyInstances: ready[s.Name],
			Age:            formatAge(updatedAt),
			StatusReason:   s.StatusReason,
			StatusMessage:  s.StatusMessage,
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

// renderStatusText is the human-readable renderer: optional roll-up, then
// per-namespace blocks. With -A we render a one-line summary per namespace
// unless --detail asks for the full per-service table.
func renderStatusText(w *os.File, report *statusReport, opts *statusOptions) error {
	if opts.allNamespaces {
		return renderAllNamespaces(w, report, opts)
	}
	if len(report.Namespaces) == 0 {
		fmt.Fprintln(w, "No namespaces found")
		return nil
	}
	nr := report.Namespaces[0]
	renderHeader(w, nr, opts.noRollUp)
	renderServiceTable(w, nr.Services)
	return nil
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

	rows := [][]string{{"NAMESPACE", "SERVICES", "RUNNING", "DEPLOYING", "STOPPING", "FAILED", "PENDING"}}
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
		})
	}
	return statusTable().WithData(rows).Render()
}

// renderHeader prints the namespace header and the bucketed roll-up.
// Buckets with zero services are omitted to keep the eye on what's actually
// present — e.g. a healthy namespace just shows "✓ Running 12", not five
// rows of zeros.
func renderHeader(w *os.File, nr namespaceReport, hideRollUp bool) {
	fmt.Fprintf(w, "Namespace: %s   ·   %d services   ·   %d instances\n",
		format.Highlight("%s", nr.Namespace), nr.Summary.Total, nr.Summary.Instances)
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
	fmt.Fprintln(w)
}

// renderServiceTable prints the per-service rows. Reason/Message only
// populated for non-Running rows so the column stays narrow when everything
// is healthy. Uses pterm so colored cells line up correctly (tabwriter
// counts ANSI escape bytes as visible width and misaligns).
func renderServiceTable(_ *os.File, services []serviceReport) {
	rows := [][]string{{"NAME", "STATUS", "SCALE", "AGE", "REASON / MESSAGE"}}
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
		rows = append(rows, []string{
			s.Name,
			colorStatus(s.Status),
			fmt.Sprintf("%d/%d", s.DesiredScale, s.ReadyInstances),
			s.Age,
			reason,
		})
	}
	_ = statusTable().WithData(rows).Render()
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
