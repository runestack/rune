// Package cmd: `rune get events` — surface the persisted event log
// (RUNE-126 Phase 2). The same log is also folded into
// `rune describe`'s Events block.
package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"text/tabwriter"
)

// handleEventsGet implements `rune get events [--for <kind>/<name>] [-n <ns>]`.
func handleEventsGet(opts *getOptions) error {
	apiClient, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	var (
		forKind, forName string
	)
	if opts.forResource != "" {
		parts := strings.SplitN(opts.forResource, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("--for must be <kind>/<name>, got %q", opts.forResource)
		}
		forKind, forName = parts[0], parts[1]
	}

	limit := opts.limit
	if limit <= 0 {
		limit = 50
	}

	evs, err := client.NewEventClient(apiClient).ListEvents(
		effectiveCmdNS(opts.namespace), forKind, forName, limit,
	)
	if err != nil {
		return err
	}

	if len(evs) == 0 {
		fmt.Println("No events.")
		return nil
	}
	return renderEventsTable(evs)
}

// renderEventsTable prints events newest first as a TIME LEVEL TARGET
// REASON MESSAGE table. Keep it deliberately plain — describe is the
// rich view, get events is the firehose.
func renderEventsTable(evs []*generated.Event) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tLEVEL\tTARGET\tCOUNT\tREASON\tMESSAGE")
	for _, e := range evs {
		count := ""
		if e.Count > 1 {
			count = fmt.Sprintf("×%d", e.Count)
		}
		target := e.Kind + "/" + e.Name
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			e.LastSeen, e.Level, target, count, e.Reason, e.Message,
		)
	}
	return w.Flush()
}
