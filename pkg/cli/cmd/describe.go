// Package cmd: `rune describe` — one-shot resource diagnostics (RUNE-126).
//
// describe is the deep "why is this resource stuck" view: status with a
// real reason, the related resources it depends on, and (Phase 2) an
// event trail. The server assembles it in one RPC; this file is a thin
// renderer over the result.
package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type describeOptions struct {
	namespace       string
	outputFormat    string
	addressOverride string
}

// newDescribeCmd builds the `rune describe` command.
func newDescribeCmd() *cobra.Command {
	opts := &describeOptions{}
	cmd := &cobra.Command{
		Use:   "describe <type>/<name> | <type> <name>",
		Short: "Show a one-shot diagnostic view of a resource",
		Long: `Describe assembles everything you need to debug a stuck resource on
one screen: its status with a real reason, the related resources it
depends on, and suggested next commands.

Supported types: instance (inst), service (svc), volume (vol).`,
		Example: `  rune describe instance flo-0 -n shared
  rune describe service flo -n shared
  rune describe volume/flo-data-flo-0 -n shared
  rune describe instance flo-0 -n shared -o yaml`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDescribe(opts, args)
		},
	}
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "", "Namespace of the resource")
	cmd.Flags().StringVarP(&opts.outputFormat, "output", "o", "table", "Output format: table|json|yaml")
	cmd.Flags().StringVar(&opts.addressOverride, "api-server", "", "Address of the API server")
	return cmd
}

// runDescribe parses the target, calls the Describe RPC and renders it.
func runDescribe(opts *describeOptions, args []string) error {
	kind, name, err := parseDescribeTarget(args)
	if err != nil {
		return err
	}

	apiClient, err := newAPIClient(opts.addressOverride, "")
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	result, err := client.NewDescribeClient(apiClient).Describe(kind, name, effectiveCmdNS(opts.namespace))
	if err != nil {
		return err
	}

	switch strings.ToLower(opts.outputFormat) {
	case "json":
		b, err := json.MarshalIndent(describeResultToOut(result), "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	case "yaml":
		b, err := yaml.Marshal(describeResultToOut(result))
		if err != nil {
			return err
		}
		fmt.Print(string(b))
	case "table", "":
		renderDescribe(os.Stdout, result)
	default:
		return fmt.Errorf("unsupported output format %q (use table|json|yaml)", opts.outputFormat)
	}
	return nil
}

// parseDescribeTarget accepts both `describe <type> <name>` and
// `describe <type>/<name>`, returning the canonical kind and the name.
func parseDescribeTarget(args []string) (kind, name string, err error) {
	switch len(args) {
	case 1:
		parts := strings.SplitN(args[0], "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("expected <type>/<name> or <type> <name>, got %q", args[0])
		}
		kind, name = parts[0], parts[1]
	case 2:
		kind, name = args[0], args[1]
	default:
		return "", "", fmt.Errorf("expected 1 or 2 arguments")
	}
	canon, ok := describeKind(kind)
	if !ok {
		return "", "", fmt.Errorf("cannot describe %q (supported: instance, service, volume)", kind)
	}
	return canon, name, nil
}

// describeKind normalizes a user-supplied type (and its aliases) to the
// canonical kind the Describe RPC expects. Reuses the shared
// resourceAliases table so `inst`/`svc`/`vol` work as in `rune get`.
func describeKind(t string) (string, bool) {
	switch canon := resourceAliases[strings.ToLower(t)]; canon {
	case "instance", "service", "volume":
		return canon, true
	}
	switch strings.ToLower(t) {
	case "node", "nodes":
		return "node", true
	}
	return "", false
}

// --- human renderer ------------------------------------------------------

// renderDescribe prints the result in the default human format.
func renderDescribe(w io.Writer, r *generated.DescribeResult) {
	fmt.Fprintf(w, "%s  %s\n", bold(r.Name), dim(r.Kind))
	if r.Namespace != "" {
		fmt.Fprintf(w, "Namespace:       %s\n", r.Namespace)
	}
	for _, kv := range r.Identity {
		if kv.Value == "" {
			continue
		}
		fmt.Fprintf(w, "%-16s %s\n", kv.Key+":", kv.Value)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "%-16s %s\n", "Status:", colorizeDescribeStatus(r.Status))
	if r.Reason != "" {
		fmt.Fprintf(w, "%-16s %s\n", "Reason:", r.Reason)
	}
	if r.Message != "" {
		fmt.Fprintf(w, "%-16s %s\n", "Message:", r.Message)
	}

	if len(r.Timestamps) > 0 {
		fmt.Fprintln(w)
		for _, kv := range r.Timestamps {
			fmt.Fprintf(w, "%-16s %s\n", kv.Key+":", kv.Value)
		}
	}

	for _, sec := range r.Sections {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s:\n", sec.Title)
		for _, line := range sec.Lines {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}

	if len(r.References) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "References:")
		for _, ref := range r.References {
			renderReference(w, ref)
		}
	}

	fmt.Fprintln(w)
	if len(r.Events) == 0 {
		fmt.Fprintf(w, "Events:          %s\n", dim("(none yet — RUNE-126 Phase 2)"))
	} else {
		fmt.Fprintln(w, "Events:")
		for _, e := range r.Events {
			fmt.Fprintf(w, "  %s  %-4s  %s\n", e.Timestamp, e.Level, e.Message)
		}
	}

	for _, h := range r.Hints {
		fmt.Fprintf(w, "  %s\n", dim(h))
	}
}

// renderReference prints one related-resource line, plus a drill-down
// `rune describe` hint when the reference is unhealthy or unresolvable.
func renderReference(w io.Writer, ref *generated.DescribeReference) {
	target := ref.Kind + "/" + ref.Name
	statusWord := ""
	switch {
	case ref.Unresolved:
		statusWord = red("unresolved")
	case ref.Status != "":
		statusWord = colorizeDescribeStatus(ref.Status)
	}
	fmt.Fprintf(w, "  %-13s %-28s %s\n", ref.Relation, target, statusWord)
	if ref.Detail != "" {
		fmt.Fprintf(w, "    %s\n", dim(ref.Detail))
	}
	if (ref.Unresolved || isUnhealthyWord(ref.Status)) && ref.Kind != "" && ref.Name != "" {
		hint := fmt.Sprintf("rune describe %s %s", ref.Kind, ref.Name)
		if ref.Namespace != "" {
			hint += " -n " + ref.Namespace
		}
		fmt.Fprintf(w, "    %s %s\n", dim("→"), dim(hint))
	}
}

// colorizeDescribeStatus colours a status word green/red/yellow.
func colorizeDescribeStatus(s string) string {
	switch {
	case s == "":
		return ""
	case isHealthyWord(s):
		return green(s)
	case isUnhealthyWord(s):
		return red(s)
	default:
		return yellow(s)
	}
}

func isHealthyWord(s string) bool {
	switch s {
	case "Running", "Available", "Bound", "Ready", "exists":
		return true
	}
	return false
}

func isUnhealthyWord(s string) bool {
	switch s {
	case "Failed", "Stalled", "Exited", "Unknown", "NotReady":
		return true
	}
	return false
}

// --- machine output ------------------------------------------------------

type describeKVOut struct {
	Key   string `json:"key" yaml:"key"`
	Value string `json:"value" yaml:"value"`
}

type describeSectionOut struct {
	Title string   `json:"title" yaml:"title"`
	Lines []string `json:"lines" yaml:"lines"`
}

type describeReferenceOut struct {
	Relation     string `json:"relation" yaml:"relation"`
	Kind         string `json:"kind" yaml:"kind"`
	Name         string `json:"name" yaml:"name"`
	Namespace    string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Status       string `json:"status,omitempty" yaml:"status,omitempty"`
	StatusReason string `json:"statusReason,omitempty" yaml:"statusReason,omitempty"`
	Detail       string `json:"detail,omitempty" yaml:"detail,omitempty"`
	Unresolved   bool   `json:"unresolved,omitempty" yaml:"unresolved,omitempty"`
}

type describeEventOut struct {
	Timestamp string `json:"timestamp" yaml:"timestamp"`
	Level     string `json:"level" yaml:"level"`
	Message   string `json:"message" yaml:"message"`
}

// describeOut is the plain, marshal-friendly mirror of the proto
// DescribeResult — used for `-o json|yaml` so the output carries no
// protobuf internals.
type describeOut struct {
	Kind       string                 `json:"kind" yaml:"kind"`
	Name       string                 `json:"name" yaml:"name"`
	Namespace  string                 `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Identity   []describeKVOut        `json:"identity,omitempty" yaml:"identity,omitempty"`
	Status     string                 `json:"status" yaml:"status"`
	Reason     string                 `json:"reason,omitempty" yaml:"reason,omitempty"`
	Message    string                 `json:"message,omitempty" yaml:"message,omitempty"`
	Timestamps []describeKVOut        `json:"timestamps,omitempty" yaml:"timestamps,omitempty"`
	Sections   []describeSectionOut   `json:"sections,omitempty" yaml:"sections,omitempty"`
	References []describeReferenceOut `json:"references,omitempty" yaml:"references,omitempty"`
	Events     []describeEventOut     `json:"events,omitempty" yaml:"events,omitempty"`
	Hints      []string               `json:"hints,omitempty" yaml:"hints,omitempty"`
}

func describeResultToOut(r *generated.DescribeResult) describeOut {
	out := describeOut{
		Kind: r.Kind, Name: r.Name, Namespace: r.Namespace,
		Status: r.Status, Reason: r.Reason, Message: r.Message,
		Hints: r.Hints,
	}
	for _, kv := range r.Identity {
		out.Identity = append(out.Identity, describeKVOut{Key: kv.Key, Value: kv.Value})
	}
	for _, kv := range r.Timestamps {
		out.Timestamps = append(out.Timestamps, describeKVOut{Key: kv.Key, Value: kv.Value})
	}
	for _, sec := range r.Sections {
		out.Sections = append(out.Sections, describeSectionOut{Title: sec.Title, Lines: sec.Lines})
	}
	for _, ref := range r.References {
		out.References = append(out.References, describeReferenceOut{
			Relation: ref.Relation, Kind: ref.Kind, Name: ref.Name, Namespace: ref.Namespace,
			Status: ref.Status, StatusReason: ref.StatusReason, Detail: ref.Detail,
			Unresolved: ref.Unresolved,
		})
	}
	for _, e := range r.Events {
		out.Events = append(out.Events, describeEventOut{Timestamp: e.Timestamp, Level: e.Level, Message: e.Message})
	}
	return out
}
