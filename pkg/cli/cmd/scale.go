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
	scaleDetach       bool
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
	scaleCmd.Flags().BoolVarP(&scaleDetach, "detach", "d", false, "Don't wait for the scaling operation to complete (fire-and-forget)")
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

	// Compose the header line. The renderer handles per-poll progress;
	// here we just announce the operation.
	label := "Scaling"
	header := "scaling"
	if scaleMode == "gradual" {
		req.Mode = generated.ScalingMode_SCALING_MODE_GRADUAL
		req.StepSize = utils.ToInt32NonNegative(scaleStep)
		req.IntervalSeconds = utils.ToInt32NonNegative(int(scaleInterval.Seconds()))
		label = fmt.Sprintf("Scaling (gradual, step=%d, interval=%s)", scaleStep, scaleInterval)
		header = fmt.Sprintf("scaling (gradual, step=%d, interval=%s)", scaleStep, scaleInterval)
	} else {
		req.Mode = generated.ScalingMode_SCALING_MODE_IMMEDIATE
	}

	fmt.Printf("↻ %s %s in %s (%d → %d)\n",
		header,
		format.Highlight("%s", serviceName),
		format.Highlight("%s", scaleNamespace),
		currentScale, replicas)

	// Send the scale request to the server
	ctx, cancel := context.WithTimeout(context.Background(), scaleTimeout)
	defer cancel()

	if _, err := serviceClient.ScaleServiceWithRequest(req); err != nil {
		return fmt.Errorf("failed to scale service: %w", err)
	}

	if scaleDetach {
		// Fire-and-forget: print a short note and exit.
		fmt.Printf("  %s detached (use `rune status %s -n %s` to check)\n",
			format.Dim("→"), serviceName, scaleNamespace)
		return nil
	}

	renderer := newPhaseRenderer(label, replicas)
	if err := waitForScalingComplete(apiClient, ctx, serviceName, scaleNamespace, replicas, renderer); err != nil {
		return err
	}
	return nil
}

// waitForScalingComplete waits for scaling operations to complete, rendering
// progress via the given phaseRenderer. The renderer's Finish() is called
// exactly once: on success, on server-reported error, on timeout, or on
// premature stream close. Callers should not call Finish() themselves.
func waitForScalingComplete(apiClient *client.Client, ctx context.Context, serviceName, namespace string, targetScale int, renderer *phaseRenderer) error {
	serviceClient := client.NewServiceClient(apiClient)

	statusCh, cancelWatch, err := serviceClient.WatchScaling(namespace, serviceName, targetScale)
	if err != nil {
		renderer.finish(false, "watch failed")
		return fmt.Errorf("failed to watch scaling: %w", err)
	}
	defer cancelWatch()

	renderer.start()

	for {
		select {
		case <-ctx.Done():
			renderer.finish(false, "timeout")
			return fmt.Errorf("timeout waiting for service to scale to %d instances", targetScale)
		case status, ok := <-statusCh:
			if !ok {
				renderer.finish(false, "stream ended")
				return fmt.Errorf("scaling operation ended without completion notification")
			}
			if status.Status != nil && status.Status.Code != 0 {
				renderer.finish(false, status.Status.Message)
				return fmt.Errorf("scaling error: %s", status.Status.Message)
			}
			renderer.update(status.RunningInstances, status.PendingInstances)
			if status.Complete {
				renderer.finish(true, "")
				return nil
			}
		}
	}
}
