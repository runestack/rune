package cmd

import (
	"fmt"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)


// statusOptions holds the options for the status subcommand
type statusOptions struct {
	cmdOptions
}

// newStatusCmd creates the status command
func newStatusCmd() *cobra.Command {
	opts := &statusOptions{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show status summary for services",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.namespace = effectiveCmdNS(opts.namespace)
			return runStatus(cmd, args, opts)
		},
	}

	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Namespace to summarize")
	cmd.Flags().StringVar(&opts.addressOverride, "api-server", "", "Address of the API server")

	return cmd
}

func init() { rootCmd.AddCommand(newStatusCmd()) }

func runStatus(cmd *cobra.Command, args []string, opts *statusOptions) error {
	api, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer api.Close()

	namespace := types.NS(opts.namespace)
	sc := client.NewServiceClient(api)
	resp, err := sc.ListServices(namespace, "", "")
	if err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	if len(resp) == 0 {
		fmt.Printf("No services found in namespace %s\n", namespace)
		return nil
	}

	// Fetch instances once for the namespace and count the ready ones per
	// service so we can render "desired/ready" instead of just desired.
	// Don't fail status if the instance list errors — services still render.
	ready := map[string]int{}
	ic := client.NewInstanceClient(api)
	if insts, err := ic.ListInstances(namespace, "", "", ""); err == nil {
		for _, inst := range insts {
			if inst == nil || inst.Status != types.InstanceStatusRunning {
				continue
			}
			ready[inst.ServiceName]++
		}
	}

	fmt.Printf("Services in %s:\n", namespace)
	fmt.Printf("%-24s %-12s %-8s\n", "NAME", "STATUS", "SCALE")
	for _, s := range resp {
		// "desired/ready". During a restart this reads naturally:
		//   1/1 → 0/1 (drain in flight) → 0/0 → 1/0 (start in flight) → 1/1
		scale := fmt.Sprintf("%d/%d", s.Scale, ready[s.Name])
		fmt.Printf("%-24s %-12s %-8s\n", s.Name, string(s.Status), scale)
	}
	return nil
}
