package cmd

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/utils"
	"github.com/spf13/cobra"
)

var (
	// Scale command flags
	scaleNamespace    string
	scaleMode         string
	scaleStep         int
	scaleInterval     time.Duration
	scaleRollbackFail bool
	scaleWait         bool
	scaleNoWait       bool
	scaleTimeout      time.Duration
	scaleClientAddr   string
)

// scaleCmd represents the scale command
var scaleCmd = &cobra.Command{
	Use:   "scale <service-name> <replicas>",
	Short: "Scale a service",
	Long: `Scale a service to the specified number of instances.

For example:
  rune scale my-service 3
  rune scale my-service 5 --namespace=production
  rune scale my-service 10 --mode=gradual --step=2 --interval=1m
  rune scale my-service 0 --no-wait`,
	Args:          cobra.ExactArgs(2),
	RunE:          runScale,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.AddCommand(scaleCmd)

	// Define flags
	scaleCmd.Flags().StringVarP(&scaleNamespace, "namespace", "n", "default", "Namespace of the service")
	scaleCmd.Flags().StringVar(&scaleMode, "mode", "immediate", "Scaling mode: 'immediate' or 'gradual'")
	scaleCmd.Flags().IntVar(&scaleStep, "step", 1, "Number of instances to add/remove per step in gradual mode")
	scaleCmd.Flags().DurationVar(&scaleInterval, "interval", 30*time.Second, "Time between steps in gradual mode")
	scaleCmd.Flags().BoolVar(&scaleRollbackFail, "rollback-on-fail", true, "Automatically rollback to previous scale on failure")
	scaleCmd.Flags().BoolVar(&scaleWait, "wait", true, "Wait for scaling operation to complete")
	scaleCmd.Flags().BoolVar(&scaleNoWait, "no-wait", false, "Don't wait for scaling operation (overrides --wait)")
	scaleCmd.Flags().DurationVar(&scaleTimeout, "timeout", 5*time.Minute, "Timeout for the wait operation")

	// API client flags
	scaleCmd.Flags().StringVar(&scaleClientAddr, "api-server", "", "Address of the API server")
}

// runScale is the main entry point for the scale command
func runScale(cmd *cobra.Command, args []string) error {
	// Parse arguments
	serviceName := args[0]
	replicas, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("invalid replicas value: %w", err)
	}

	// Validate input
	if replicas < 0 {
		return fmt.Errorf("replicas must be a non-negative integer")
	}

	// The --no-wait flag overrides the --wait flag
	waitForCompletion := scaleWait && !scaleNoWait

	// Validate scaling mode
	if scaleMode != "immediate" && scaleMode != "gradual" {
		return fmt.Errorf("invalid scaling mode: %s (must be 'immediate' or 'gradual')", scaleMode)
	}

	// Create API client
	apiClient, err := newAPIClient(scaleClientAddr, "")
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	// Create service client
	serviceClient := client.NewServiceClient(apiClient)

	// Get current service to validate it exists and get current scale
	currentService, err := serviceClient.GetService(scaleNamespace, serviceName)
	if err != nil {
		return fmt.Errorf("failed to get service %s/%s: %w", scaleNamespace, serviceName, err)
	}

	// Get current scale for logging
	currentScale := currentService.Scale

	// Create a new scale request
	req := &generated.ScaleServiceRequest{
		Name:      serviceName,
		Namespace: scaleNamespace,
		Scale:     utils.ToInt32NonNegative(replicas),
	}

	// Handle scaling mode
	if scaleMode == "gradual" {
		// Set gradual scaling parameters
		req.Mode = generated.ScalingMode_SCALING_MODE_GRADUAL
		req.StepSize = utils.ToInt32NonNegative(scaleStep)
		req.IntervalSeconds = utils.ToInt32NonNegative(int(scaleInterval.Seconds()))

		fmt.Printf("Gradually scaling service %s from %d to %d instances (step size: %d, interval: %s)\n",
			format.Highlight(serviceName), currentScale, replicas, scaleStep, scaleInterval)
	} else {
		// Immediate scaling
		req.Mode = generated.ScalingMode_SCALING_MODE_IMMEDIATE

		fmt.Printf("Scaling service %s from %d to %d instances\n",
			format.Highlight(serviceName), currentScale, replicas)
	}

	// Send the scale request to the server
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout)
	defer cancel()

	resp, err := serviceClient.ScaleServiceWithRequest(req)
	if err != nil {
		return fmt.Errorf("failed to scale service: %w", err)
	}

	fmt.Printf("Successfully initiated scaling: %s\n", resp.Status.Message)

	// If wait is requested, poll the service until it reaches the target scale
	if waitForCompletion {
		fmt.Printf("Waiting for scaling operation to complete (timeout: %s)...\n", scaleTimeout)

		err = waitForScalingComplete(apiClient, ctx, serviceName, scaleNamespace, replicas)
		if err != nil {
			return fmt.Errorf("error while waiting for scaling: %w", err)
		}

		fmt.Printf("%s Service %s successfully scaled to %d instances\n",
			format.Success("✓"), format.Highlight(serviceName), replicas)
	}

	return nil
}

// waitForScalingComplete waits for scaling operations to complete
func waitForScalingComplete(apiClient *client.Client, ctx context.Context, serviceName, namespace string, targetScale int) error {
	// Create service client
	serviceClient := client.NewServiceClient(apiClient)

	// Use the new WatchScaling method to get real-time updates
	statusCh, cancelWatch, err := serviceClient.WatchScaling(namespace, serviceName, targetScale)
	if err != nil {
		return fmt.Errorf("failed to watch scaling: %w", err)
	}
	defer cancelWatch() // Ensure we clean up the stream when done

	// Process status updates from the stream
	for status := range statusCh {
		// Check for errors
		if status.Status != nil && status.Status.Code != 0 {
			return fmt.Errorf("scaling error: %s", status.Status.Message)
		}

		// Display current status
		fmt.Printf("Current status: %d/%d instances running", status.RunningInstances, targetScale)
		if status.PendingInstances > 0 {
			fmt.Printf(" (%d pending)", status.PendingInstances)
		}
		fmt.Println()

		// Check if scaling is complete
		if status.Complete {
			return nil
		}

		// Check for context cancellation (timeout)
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for service to scale to %d instances", targetScale)
		default:
			// Continue processing
		}
	}

	// If we get here, the stream ended without completion
	return fmt.Errorf("scaling operation ended without completion notification")
}
