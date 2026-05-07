// Package cmd: admin network subcommands.
//
// `rune admin network status` calls AdminService.NetworkStatus and
// renders the cluster ClusterNetwork CIDR plus VIP allocation summary
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/spf13/cobra"
)

func newAdminNetworkCmd() *cobra.Command {
	c := &cobra.Command{Use: "network", Short: "Inspect cluster network state (CIDR, VIP allocations)"}
	c.AddCommand(newAdminNetworkStatusCmd())
	return c
}

func newAdminNetworkStatusCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show cluster network CIDR, VIP allocations, and capacity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()
			ac := generated.NewAdminServiceClient(api.Conn())
			resp, err := ac.NetworkStatus(context.Background(), &generated.NetworkStatusRequest{})
			if err != nil {
				return err
			}
			if jsonOut {
				b, _ := json.MarshalIndent(resp, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if resp.Cidr == "" {
				fmt.Println("Cluster network not bootstrapped.")
				return nil
			}
			fmt.Printf("CIDR:             %s\n", resp.Cidr)
			fmt.Printf("Capacity:         %d usable IPs\n", resp.Capacity)
			fmt.Printf("Allocated:        %d\n", len(resp.Allocations))
			fmt.Printf("Free list size:   %d\n", resp.FreeListSize)
			fmt.Printf("Pending releases: %d (cooldown)\n", resp.PendingReleases)
			if len(resp.Allocations) > 0 {
				sort.Slice(resp.Allocations, func(i, j int) bool {
					return resp.Allocations[i].ServiceId < resp.Allocations[j].ServiceId
				})
				fmt.Println()
				fmt.Println("Allocations:")
				fmt.Printf("  %-40s %s\n", "SERVICE", "VIP")
				for _, a := range resp.Allocations {
					fmt.Printf("  %-40s %s\n", a.ServiceId, a.Vip)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of human-readable output")
	return cmd
}
