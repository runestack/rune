package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/utils"
	"github.com/spf13/cobra"
)

var (
	restartNamespace  string
	restartWait       bool
	restartNoWait     bool
	restartTimeout    time.Duration
	restartClientAddr string
)

// restartCmd represents the restart command (service bounce)
var restartCmd = &cobra.Command{
	Use:   "restart <service-name>",
	Short: "Restart a service by scaling it to 0 and back to its previous scale",
	Args:  cobra.ExactArgs(1),
	RunE:  runRestart,
}

func init() {
	rootCmd.AddCommand(restartCmd)

	restartCmd.Flags().StringVarP(&restartNamespace, "namespace", "n", "default", "Namespace of the service")
	restartCmd.Flags().BoolVar(&restartWait, "wait", true, "Wait for the restart to complete")
	restartCmd.Flags().BoolVar(&restartNoWait, "no-wait", false, "Don't wait for the restart to complete")
	restartCmd.Flags().DurationVar(&restartTimeout, "timeout", 10*time.Minute, "Timeout for the restart operation")

	// API client flags
	restartCmd.Flags().StringVar(&restartClientAddr, "api-server", "", "Address of the API server")
}

func runRestart(cmd *cobra.Command, args []string) error {
	serviceName := args[0]

	// Create API client
	apiClient, err := newAPIClient(restartClientAddr, "")
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	svcClient := client.NewServiceClient(apiClient)

	// Fetch current scale
	svc, err := svcClient.GetService(restartNamespace, serviceName)
	if err != nil {
		return fmt.Errorf("failed to get service %s/%s: %w", restartNamespace, serviceName, err)
	}
	current := svc.Scale
	// If stopped, restore to last non-zero scale (fallback to 1)
	if current == 0 {
		desired := svc.Metadata.LastNonZeroScale
		if desired <= 0 {
			desired = 1
		}
		fmt.Printf("Service %s is stopped; restoring to previous scale %d\n", format.Highlight(serviceName), desired)
		upReq := &generated.ScaleServiceRequest{
			Name:      serviceName,
			Namespace: restartNamespace,
			Scale:     utils.ToInt32NonNegative(desired),
			Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
		}
		if _, err := svcClient.ScaleServiceWithRequest(upReq); err != nil {
			return fmt.Errorf("failed to scale up service: %w", err)
		}
		if restartWait && !restartNoWait {
			ctx, cancel := context.WithTimeout(context.Background(), restartTimeout)
			defer cancel()
			if err := waitForScalingComplete(apiClient, ctx, serviceName, restartNamespace, desired); err != nil {
				return fmt.Errorf("error waiting for scale-up: %w", err)
			}
			fmt.Printf("%s Service %s started (%d instances)\n", format.Success("✓"), format.Highlight(serviceName), desired)
		}
		return nil
	}

	fmt.Printf("Restarting service %s by bouncing instances: %d → 0 → %d\n", format.Highlight(serviceName), current, current)

	// Scale to 0
	downReq := &generated.ScaleServiceRequest{
		Name:      serviceName,
		Namespace: restartNamespace,
		Scale:     0,
		Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
	}
	if _, err := svcClient.ScaleServiceWithRequest(downReq); err != nil {
		return fmt.Errorf("failed to scale down service: %w", err)
	}

	if restartWait && !restartNoWait {
		ctx, cancel := context.WithTimeout(context.Background(), restartTimeout)
		defer cancel()
		if err := waitForScalingComplete(apiClient, ctx, serviceName, restartNamespace, 0); err != nil {
			return fmt.Errorf("error waiting for scale-down: %w", err)
		}
	}

	// Scale back up to previous
	upReq := &generated.ScaleServiceRequest{
		Name:      serviceName,
		Namespace: restartNamespace,
		Scale:     utils.ToInt32NonNegative(current),
		Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
	}
	if _, err := svcClient.ScaleServiceWithRequest(upReq); err != nil {
		return fmt.Errorf("failed to scale up service: %w", err)
	}

	if restartWait && !restartNoWait {
		ctx, cancel := context.WithTimeout(context.Background(), restartTimeout)
		defer cancel()
		if err := waitForScalingComplete(apiClient, ctx, serviceName, restartNamespace, current); err != nil {
			return fmt.Errorf("error waiting for scale-up: %w", err)
		}
		fmt.Printf("%s Service %s restarted (%d instances)\n", format.Success("✓"), format.Highlight(serviceName), current)
	}

	return nil
}
