package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// newAuditCmd builds the `rune audit` command group, used by operators to
// inspect the server-side security audit trail (RUNE-102).
func newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Inspect the server-side security audit log",
	}
	cmd.AddCommand(newAuditListCmd())
	return cmd
}

func newAuditListCmd() *cobra.Command {
	var (
		resource    string
		ref         string
		namespace   string
		actor       string
		action      string
		sinceStr    string
		untilStr    string
		limit       int
		outFormat   string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List audit events, newest first",
		Long: `List audit events from the server-side audit log.

By default returns up to 200 events. Use --limit to override (capped server-side).
Time windows accept either an absolute RFC3339 timestamp (e.g. 2025-01-02T15:04:05Z)
or a relative duration like "24h", "30m", "7d" (interpreted as "now minus that").`,
		RunE: func(cmd *cobra.Command, args []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()

			ac := client.NewAuditClient(api)

			opts := client.AuditListOptions{
				Resource:    resource,
				ResourceRef: ref,
				Namespace:   namespace,
				Actor:       actor,
				Action:      action,
				Limit:       limit,
			}
			if sinceStr != "" {
				t, err := parseTimeOrDuration(sinceStr)
				if err != nil {
					return fmt.Errorf("--since: %w", err)
				}
				opts.Since = t
			}
			if untilStr != "" {
				t, err := parseTimeOrDuration(untilStr)
				if err != nil {
					return fmt.Errorf("--until: %w", err)
				}
				opts.Until = t
			}

			events, err := ac.ListAuditEvents(opts)
			if err != nil {
				return err
			}

			switch outFormat {
			case "json":
				return json.NewEncoder(os.Stdout).Encode(events)
			case "yaml":
				return yaml.NewEncoder(os.Stdout).Encode(events)
			default:
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "TIMESTAMP\tACTOR\tACTION\tRESOURCE\tREF\tOUTCOME\tMESSAGE")
				for _, e := range events {
					msg := e.Message
					if len(msg) > 60 {
						msg = msg[:57] + "..."
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
						e.Timestamp.UTC().Format(time.RFC3339),
						orDash(e.Actor),
						e.Action,
						e.Resource,
						orDash(e.ResourceRef),
						string(e.Outcome),
						msg,
					)
				}
				return w.Flush()
			}
		},
	}

	cmd.Flags().StringVar(&resource, "resource", "", "filter by resource type (e.g. secrets)")
	cmd.Flags().StringVar(&ref, "ref", "", `filter by resource ref ("namespace/name")`)
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "filter by namespace")
	cmd.Flags().StringVar(&actor, "actor", "", "filter by actor (subject ID)")
	cmd.Flags().StringVar(&action, "action", "", "filter by action (get|create|update|delete|reveal|...)")
	cmd.Flags().StringVar(&sinceStr, "since", "", "only events at/after this time (RFC3339 or duration like 24h)")
	cmd.Flags().StringVar(&untilStr, "until", "", "only events before this time (RFC3339 or duration like 1h)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max events to return (0 = server default of 200)")
	cmd.Flags().StringVarP(&outFormat, "output", "o", "table", "output format: table|json|yaml")
	return cmd
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// parseTimeOrDuration accepts either an RFC3339 timestamp or a Go duration
// string like "24h", "30m", "7d". For durations the returned time is
// (time.Now() - dur), i.e. "events from the last <dur>".
//
// "d" suffix is supported as a convenience shorthand for "24h".
func parseTimeOrDuration(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Treat trailing 'd' as a multiple of 24h.
	dur := s
	if strings.HasSuffix(s, "d") {
		days, err := time.ParseDuration(strings.TrimSuffix(s, "d") + "h")
		if err == nil {
			return time.Now().Add(-days * 24), nil
		}
	}
	d, err := time.ParseDuration(dur)
	if err != nil {
		return time.Time{}, fmt.Errorf("not a valid RFC3339 timestamp or duration: %q", s)
	}
	return time.Now().Add(-d), nil
}
