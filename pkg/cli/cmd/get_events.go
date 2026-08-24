// Package cmd: `rune get events` — surface the persisted event log
// (RUNE-126 Phase 2). The same log is also folded into
// `rune describe`'s Events block.
package cmd

import (
	"fmt"
	"strings"

	"github.com/runestack/rune/pkg/api/client"
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

	// The caller's namespace still goes on the wire, including for
	// node/<name>: it is the RBAC input, and a namespace-pinned grant is
	// allowed to read a node's (cluster-scoped) events. The server
	// rescopes the QUERY for node — see EventService.ListEvents.
	ns := effectiveCmdNS(opts.namespace)

	evs, err := client.NewEventClient(apiClient).ListEvents(
		ns, forKind, forName, limit,
	)
	if err != nil {
		return err
	}

	table := NewResourceTable()
	table.Namespace = ns
	if strings.EqualFold(forKind, "node") {
		// Cluster-scoped: labelling the table with a namespace the rows
		// do not have would be a lie.
		table.Namespace = ""
	}
	table.ShowHeaders = !opts.noHeaders
	return table.RenderEvents(evs)
}
