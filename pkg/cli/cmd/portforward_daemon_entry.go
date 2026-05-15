package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/runestack/rune/pkg/api/client"
	pfdaemon "github.com/runestack/rune/pkg/cli/cmd/portforward_daemon"
	"github.com/runestack/rune/pkg/log"
	"github.com/spf13/cobra"
)

// newPortForwardDaemonEntryCmd is the hidden subcommand that
// implements `rune __port-forward-daemon`. It's invoked by
// `rune port-forward -d` to spawn the long-lived daemon process.
//
// Hidden because it isn't part of the user-facing surface — the
// double-underscore convention signals "internal plumbing."
func newPortForwardDaemonEntryCmd() *cobra.Command {
	opts := &cmdOptions{}
	cmd := &cobra.Command{
		Use:    "__port-forward-daemon",
		Hidden: true,
		RunE: func(c *cobra.Command, args []string) error {
			return runPortForwardDaemon(opts)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.Flags().StringVar(&opts.addressOverride, "server", "", "API server address")
	cmd.Flags().StringVar(&opts.tokenOverride, "token", "", "API token")
	return cmd
}

func init() { rootCmd.AddCommand(newPortForwardDaemonEntryCmd()) }

func runPortForwardDaemon(opts *cmdOptions) error {
	dir, err := pfdaemon.StateDir()
	if err != nil {
		return err
	}

	logger := log.NewLogger().WithComponent("pf-daemon")

	newClient := func() (*client.Client, error) {
		return createAPIClient(opts)
	}

	d := pfdaemon.New(dir, logger, newClient)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Catch SIGTERM/SIGINT so a `kill` of the daemon shuts down
	// cleanly (removes the socket, releases the pidfile lock).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "pf-daemon: signal received, shutting down")
		cancel()
	}()

	return d.Run(ctx)
}
