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
	stopWait       bool
	stopNoWait     bool
	stopTimeout    time.Duration
	stopClientAddr string
)

// stopCmd represents the stop command (service)
var stopCmd = &cobra.Command{
	Use:   "stop <service-name>",
	Short: "Stop a service by scaling it down to 0",
	Args:  cobra.ExactArgs(1),
	RunE:  runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)

	stopCmd.Flags().StringVarP(&stopNamespace, "namespace", "n", "default", "Namespace of the service")
	stopCmd.Flags().BoolVar(&stopWait, "wait", true, "Wait for service to fully stop")
	stopCmd.Flags().BoolVar(&stopNoWait, "no-wait", false, "Don't wait for service to stop")
	stopCmd.Flags().DurationVar(&stopTimeout, "timeout", 5*time.Minute, "Timeout for wait operation")

	// API client flags
	stopCmd.Flags().StringVar(&stopClientAddr, "api-server", "", "Address of the API server")
}

func runStop(cmd *cobra.Command, args []string) error {
	serviceName := args[0]

	// Create API client
	apiClient, err := newAPIClient(stopClientAddr, "")
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	svcClient := client.NewServiceClient(apiClient)

	// Validate service exists
	svc, err := svcClient.GetService(stopNamespace, serviceName)
	if err != nil {
		return fmt.Errorf("failed to get service %s/%s: %w", stopNamespace, serviceName, err)
	}

	fmt.Printf("Stopping service %s (current scale: %d) → 0\n", format.Highlight(serviceName), svc.Scale)

	req := &generated.ScaleServiceRequest{
		Name:      serviceName,
		Namespace: stopNamespace,
		Scale:     0,
		Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
	}

	if _, err := svcClient.ScaleServiceWithRequest(req); err != nil {
		return fmt.Errorf("failed to stop service: %w", err)
	}

	if stopWait && !stopNoWait {
		ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		if err := waitForScalingComplete(apiClient, ctx, serviceName, stopNamespace, 0); err != nil {
			return err
		}
		fmt.Printf("%s Service %s stopped\n", format.Success("✓"), format.Highlight(serviceName))
	}

	return nil
}
