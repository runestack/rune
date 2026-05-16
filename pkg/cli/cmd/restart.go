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
	restartDetach     bool
	restartTimeout    time.Duration
	restartClientAddr string
)

// restartCmd represents the restart command (service bounce).
var restartCmd = &cobra.Command{
	Use:   "restart <service-name>",
	Short: "Restart a service by scaling it to 0 and back to its previous scale",
	Args:  cobra.ExactArgs(1),
	RunE:  runRestart,
}

func init() {
	rootCmd.AddCommand(restartCmd)

	restartCmd.Flags().StringVarP(&restartNamespace, "namespace", "n", "default", "Namespace of the service")
	restartCmd.Flags().BoolVarP(&restartDetach, "detach", "d", false, "Don't wait for the restart to complete (fire-and-forget)")
	restartCmd.Flags().DurationVar(&restartTimeout, "timeout", 10*time.Minute, "Timeout for the restart operation")
	restartCmd.Flags().StringVar(&restartClientAddr, "api-server", "", "Address of the API server")
}

func runRestart(cmd *cobra.Command, args []string) error {
	serviceName := args[0]

	apiClient, err := newAPIClient(restartClientAddr, "")
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	svcClient := client.NewServiceClient(apiClient)

	svc, err := svcClient.GetService(restartNamespace, serviceName)
	if err != nil {
		return fmt.Errorf("failed to get service %s/%s: %w", restartNamespace, serviceName, err)
	}
	current := svc.Scale

	// Stopped → just scale up to the last non-zero scale.
	if current == 0 {
		desired := svc.Metadata.LastNonZeroScale
		if desired <= 0 {
			desired = 1
		}
		fmt.Printf("↻ Starting %s in %s (stopped → %d)\n",
			format.Highlight("%s", serviceName),
			format.Highlight("%s", restartNamespace),
			desired)
		return doScale(apiClient, svcClient, serviceName, restartNamespace, desired,
			"Starting", restartDetach, restartTimeout)
	}

	// Normal restart: drain to 0, then scale back up.
	fmt.Printf("↻ Restarting %s in %s (%d → 0 → %d)\n",
		format.Highlight("%s", serviceName),
		format.Highlight("%s", restartNamespace),
		current, current)

	if restartDetach {
		// Fire and forget. We still need to send the scale-down request and
		// queue a scale-up; the orchestrator handles that as one operation.
		// For simplicity (and since the orchestrator currently treats these as
		// independent), do scale-down now and warn that scale-up requires a
		// follow-up. Most users will just want the synchronous flow.
		downReq := &generated.ScaleServiceRequest{
			Name:      serviceName,
			Namespace: restartNamespace,
			Scale:     0,
			Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
		}
		if _, err := svcClient.ScaleServiceWithRequest(downReq); err != nil {
			return fmt.Errorf("failed to scale down service: %w", err)
		}
		upReq := &generated.ScaleServiceRequest{
			Name:      serviceName,
			Namespace: restartNamespace,
			Scale:     utils.ToInt32NonNegative(current),
			Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
		}
		if _, err := svcClient.ScaleServiceWithRequest(upReq); err != nil {
			return fmt.Errorf("failed to queue scale up: %w", err)
		}
		fmt.Printf("  %s detached (use `rune status %s -n %s` to check)\n",
			format.Dim("→"), serviceName, restartNamespace)
		return nil
	}

	wallStart := time.Now()

	// Phase 1: drain.
	downReq := &generated.ScaleServiceRequest{
		Name:      serviceName,
		Namespace: restartNamespace,
		Scale:     0,
		Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
	}
	if _, err := svcClient.ScaleServiceWithRequest(downReq); err != nil {
		return fmt.Errorf("failed to scale down service: %w", err)
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), restartTimeout)
		defer cancel()
		renderer := newPhaseRenderer("Draining", 0)
		if err := waitForScalingComplete(apiClient, ctx, serviceName, restartNamespace, 0, renderer); err != nil {
			return err
		}
	}

	// Phase 2: start back up to the previous scale.
	upReq := &generated.ScaleServiceRequest{
		Name:      serviceName,
		Namespace: restartNamespace,
		Scale:     utils.ToInt32NonNegative(current),
		Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
	}
	if _, err := svcClient.ScaleServiceWithRequest(upReq); err != nil {
		return fmt.Errorf("failed to scale up service: %w", err)
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), restartTimeout)
		defer cancel()
		renderer := newPhaseRenderer("Starting", current)
		if err := waitForScalingComplete(apiClient, ctx, serviceName, restartNamespace, current, renderer); err != nil {
			return err
		}
	}

	fmt.Printf("%s %s restarted (%s)\n",
		format.Success("✓"),
		format.Highlight("%s", serviceName),
		formatDuration(time.Since(wallStart)))
	return nil
}

// doScale is the single-phase helper used for the stopped-service code path.
// The renderer's own finish line is the summary — no extra "ready" line.
func doScale(apiClient *client.Client, svcClient *client.ServiceClient, name, namespace string, target int, label string, detach bool, timeout time.Duration) error {
	req := &generated.ScaleServiceRequest{
		Name:      name,
		Namespace: namespace,
		Scale:     utils.ToInt32NonNegative(target),
		Mode:      generated.ScalingMode_SCALING_MODE_IMMEDIATE,
	}
	if _, err := svcClient.ScaleServiceWithRequest(req); err != nil {
		return fmt.Errorf("failed to scale service: %w", err)
	}
	if detach {
		fmt.Printf("  %s detached (use `rune status %s -n %s` to check)\n",
			format.Dim("→"), name, namespace)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	renderer := newPhaseRenderer(label, target)
	return waitForScalingComplete(apiClient, ctx, name, namespace, target, renderer)
}
