package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/spf13/cobra"
)

var (
	stopNamespace  string
	stopDetach     bool
	stopTimeout    time.Duration
	stopClientAddr string
)

// stopCmd represents the stop command (service).
var stopCmd = &cobra.Command{
	Use:   "stop <service-name>",
	Short: "Stop a service by scaling it down to 0",
	Args:  cobra.ExactArgs(1),
	RunE:  runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)

	stopCmd.Flags().StringVarP(&stopNamespace, "namespace", "n", "default", "Namespace of the service")
	stopCmd.Flags().BoolVarP(&stopDetach, "detach", "d", false, "Don't wait for the service to fully stop (fire-and-forget)")
	stopCmd.Flags().DurationVar(&stopTimeout, "timeout", 5*time.Minute, "Timeout for wait operation")
	stopCmd.Flags().StringVar(&stopClientAddr, "api-server", "", "Address of the API server")
}

func runStop(cmd *cobra.Command, args []string) error {
	serviceName := args[0]

	apiClient, err := newAPIClient(stopClientAddr, "")
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	svcClient := client.NewServiceClient(apiClient)

	svc, err := svcClient.GetService(stopNamespace, serviceName)
	if err != nil {
		return fmt.Errorf("failed to get service %s/%s: %w", stopNamespace, serviceName, err)
	}

	fmt.Printf("↻ Stopping %s in %s (%d → 0)\n",
		format.Highlight("%s", serviceName),
		format.Highlight("%s", stopNamespace),
		svc.Scale)

	req := &generated.ScaleServiceRequest{
		Name:      serviceName,
		Namespace: stopNamespace,
		Scale:     0,
		Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
	}
	if _, err := svcClient.ScaleServiceWithRequest(req); err != nil {
		return fmt.Errorf("failed to stop service: %w", err)
	}

	if stopDetach {
		fmt.Printf("  %s detached (use `rune status %s -n %s` to check)\n",
			format.Dim("→"), serviceName, stopNamespace)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	renderer := newPhaseRenderer("Stopping", 0)
	return waitForScalingComplete(apiClient, ctx, serviceName, stopNamespace, 0, renderer, waitOptions{})
}
