package cmd

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	pfdaemon "github.com/runestack/rune/pkg/cli/cmd/portforward_daemon"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

// runPortForwardDetached hands a forward off to the local daemon
// instead of running it in the foreground. Spawns the daemon if it
// isn't already running. Prints the resulting forward summary.
func runPortForwardDetached(target string, mappings []portMapping, opts *portForwardOptions) error {
	dir, err := pfdaemon.StateDir()
	if err != nil {
		return err
	}

	// Resolve target via the API up front. We need the canonical
	// service vs instance distinction for the daemon's persisted
	// state — and surfacing a "no such service" error from the CLI
	// is friendlier than letting it bubble out through the daemon's
	// retry loop.
	apiClient, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return fmt.Errorf("failed to create API client: %w", err)
	}
	resolved, err := resolveResourceTarget(apiClient, target, opts.namespace)
	apiClient.Close()
	if err != nil {
		return fmt.Errorf("failed to resolve target: %w", err)
	}

	pfwd := &pfdaemon.Forward{
		Namespace:   resolved.namespace,
		Target:      resolved.target,
		InstancePin: opts.instance,
		CreatedAt:   time.Now().UTC(),
	}
	if resolved.targetType == types.ResourceTypeInstance {
		pfwd.TargetKind = pfdaemon.TargetInstance
	} else {
		pfwd.TargetKind = pfdaemon.TargetService
	}
	for _, m := range mappings {
		pfwd.Mappings = append(pfwd.Mappings, pfdaemon.PortMapping{
			Local:  net.JoinHostPort(opts.bindAddr, strconv.Itoa(int(m.local))),
			Remote: uint32(m.remote),
		})
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	sock, err := pfdaemon.EnsureRunning(dir, []string{exe, "__port-forward-daemon"})
	if err != nil {
		return err
	}

	resp, err := pfdaemon.Call(sock, pfdaemon.Request{Cmd: pfdaemon.CmdAdd, Forward: pfwd})
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("daemon: %s", resp.Error)
	}

	out := resp.Forward
	if out == nil {
		return fmt.Errorf("daemon: missing forward in response")
	}
	fmt.Fprintf(os.Stderr, "✓ Forward %s started: %s -> %s/%s (pid %d)\n",
		out.ID,
		out.Mappings[0].Local,
		out.Namespace,
		out.Target,
		daemonPid(dir))
	return nil
}

// daemonPid reads the daemon pid for display. Returns 0 if unreadable.
func daemonPid(dir string) int {
	b, err := os.ReadFile(pfdaemon.PidPath(dir))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(string(trimSpaceBytes(b)))
	return pid
}

func trimSpaceBytes(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\n' || b[i] == '\r' || b[i] == '\t') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\n' || b[j-1] == '\r' || b[j-1] == '\t') {
		j--
	}
	return b[i:j]
}

// --- list ---

func newPortForwardListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List active port-forwards owned by the daemon",
		RunE: func(c *cobra.Command, args []string) error {
			return runPortForwardList()
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
}

func runPortForwardList() error {
	dir, err := pfdaemon.StateDir()
	if err != nil {
		return err
	}
	sock := pfdaemon.SocketPath(dir)

	var forwards []*pfdaemon.Forward
	if pfdaemon.IsAlive(dir) {
		resp, err := pfdaemon.Call(sock, pfdaemon.Request{Cmd: pfdaemon.CmdList})
		if err != nil {
			return fmt.Errorf("daemon: %w", err)
		}
		if !resp.OK {
			return fmt.Errorf("daemon: %s", resp.Error)
		}
		forwards = resp.Forwards
	} else {
		// Daemon not running. Show whatever state we have on disk
		// with a `(stale)` marker so the operator knows the truth.
		on, err := pfdaemon.LoadForwards(dir)
		if err != nil {
			return err
		}
		forwards = on
		if len(forwards) == 0 {
			fmt.Fprintln(os.Stderr, "no port-forwards active")
			return nil
		}
		fmt.Fprintln(os.Stderr, "(daemon not running; showing stale state from disk)")
	}

	sort.Slice(forwards, func(i, j int) bool { return forwards[i].CreatedAt.Before(forwards[j].CreatedAt) })

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tLOCAL\tTARGET\tSTATUS\tAGE")
	for _, fwd := range forwards {
		local := ""
		if len(fwd.Mappings) > 0 {
			local = fwd.Mappings[0].Local
			if len(fwd.Mappings) > 1 {
				local += fmt.Sprintf(" (+%d)", len(fwd.Mappings)-1)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s/%s\t%s\t%s\n",
			fwd.ID, local, fwd.Namespace, fwd.Target, fwd.Status,
			humanizeAge(time.Since(fwd.CreatedAt)))
	}
	return tw.Flush()
}

func humanizeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// --- stop ---

func newPortForwardStopCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "stop [ID...]",
		Short: "Stop one or more active port-forwards",
		RunE: func(c *cobra.Command, args []string) error {
			return runPortForwardStop(args, all)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.Flags().BoolVar(&all, "all", false, "stop every active forward")
	return cmd
}

func runPortForwardStop(ids []string, all bool) error {
	dir, err := pfdaemon.StateDir()
	if err != nil {
		return err
	}
	sock := pfdaemon.SocketPath(dir)

	if !pfdaemon.IsAlive(dir) {
		fmt.Fprintln(os.Stderr, "daemon not running")
		return nil
	}

	if all {
		resp, err := pfdaemon.Call(sock, pfdaemon.Request{Cmd: pfdaemon.CmdStopAll})
		if err != nil {
			return fmt.Errorf("daemon: %w", err)
		}
		if !resp.OK {
			return fmt.Errorf("daemon: %s", resp.Error)
		}
		fmt.Fprintf(os.Stderr, "✓ Stopped %d forward(s)\n", resp.Stopped)
		return nil
	}

	if len(ids) == 0 {
		return fmt.Errorf("specify at least one ID, or use --all")
	}
	for _, id := range ids {
		resp, err := pfdaemon.Call(sock, pfdaemon.Request{Cmd: pfdaemon.CmdStop, ID: id})
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		if !resp.OK {
			return fmt.Errorf("%s: %s", id, resp.Error)
		}
		fmt.Fprintf(os.Stderr, "✓ Forward %s stopped\n", id)
	}
	return nil
}

// --- logs ---

func newPortForwardLogsCmd() *cobra.Command {
	var tail int
	cmd := &cobra.Command{
		Use:   "logs ID",
		Short: "Show recent log lines for a daemon-owned forward",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runPortForwardLogs(args[0], tail)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.Flags().IntVar(&tail, "tail", 200, "number of trailing lines to show")
	return cmd
}

func runPortForwardLogs(id string, tail int) error {
	dir, err := pfdaemon.StateDir()
	if err != nil {
		return err
	}
	if !pfdaemon.IsAlive(dir) {
		return fmt.Errorf("daemon not running")
	}
	resp, err := pfdaemon.Call(pfdaemon.SocketPath(dir), pfdaemon.Request{
		Cmd: pfdaemon.CmdLogs, ID: id, Tail: tail,
	})
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("daemon: %s", resp.Error)
	}
	for _, line := range resp.Lines {
		fmt.Println(line)
	}
	return nil
}
