package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/utils"
	"github.com/spf13/cobra"
)

var (
	// Scale command flags
	scaleNamespace         string
	scaleMode              string
	scaleStep              int
	scaleInterval          time.Duration
	scaleRollbackFail      bool
	scaleDetach            bool
	scaleTimeout           time.Duration
	scaleNoProgressTimeout time.Duration
	scaleClientAddr        string
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
	scaleCmd.Flags().DurationVar(&scaleNoProgressTimeout, "no-progress-timeout", 0, "If >0, detach after this long with no change in running/pending/failed counts (only when at least one instance is Failed). Default off — relies on --timeout and Stalled-status detection.")

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
	if err := waitForScalingComplete(apiClient, ctx, serviceName, scaleNamespace, replicas, renderer, waitOptions{
		noProgressTimeout: scaleNoProgressTimeout,
	}); err != nil {
		return err
	}
	return nil
}

// waitOptions tunes auto-detach heuristics for waitForScalingComplete.
// Renderer behavior is unchanged — these only affect the detach decision.
type waitOptions struct {
	// noProgressTimeout, if >0, causes the wait to detach when:
	//   (a) at least one instance is Failed AND
	//   (b) the (running, pending, failed, terminating, currentScale)
	//       signature has been unchanged for this long.
	// Default 0 (off) — relies on the hard --timeout and Stalled detection.
	noProgressTimeout time.Duration
}

// stuckTerminatingTimeout is the built-in safety net for drain (target=0):
// if all instances are Terminating and the signature hasn't changed for
// this long, we detach with a clear reason instead of waiting out the
// full --timeout. 90s comfortably covers the runner's default graceful
// shutdown budget plus retries; anything beyond is almost certainly stuck.
const stuckTerminatingTimeout = 90 * time.Second

// waitForScalingComplete waits for scaling operations to complete, rendering
// progress via the given phaseRenderer. The renderer's finish() is called
// exactly once: on success, on server-reported error, on timeout, on
// premature stream close, or on auto-detach. Callers should not call
// finish() themselves.
//
// Auto-detach (returns non-nil error with a problem summary) fires on:
//   - Any Stalled instance — reconciler has given up, waiting is pointless
//   - Drain (target=0): all live instances Terminating, signature stable
//     for stuckTerminatingTimeout
//   - Scale-up: Failed count >0 AND signature stable for opts.noProgressTimeout
//     (only when opt-in)
func waitForScalingComplete(apiClient *client.Client, ctx context.Context, serviceName, namespace string, targetScale int, renderer *phaseRenderer, opts waitOptions) error {
	serviceClient := client.NewServiceClient(apiClient)

	statusCh, cancelWatch, err := serviceClient.WatchScaling(namespace, serviceName, targetScale)
	if err != nil {
		renderer.finish(false, "watch failed")
		return fmt.Errorf("failed to watch scaling: %w", err)
	}
	defer cancelWatch()

	renderer.start()

	var (
		lastSignature  string
		lastChangeAt   = time.Now()
		lastSeenStatus *generated.ScalingStatusResponse
	)

	for {
		select {
		case <-ctx.Done():
			renderer.finish(false, "timeout")
			return scalingDetachError("timeout", targetScale, lastSeenStatus)

		case status, ok := <-statusCh:
			if !ok {
				renderer.finish(false, "stream ended")
				return fmt.Errorf("scaling operation ended without completion notification")
			}
			if status.Status != nil && status.Status.Code != 0 {
				renderer.finish(false, status.Status.Message)
				return fmt.Errorf("scaling error: %s", status.Status.Message)
			}

			lastSeenStatus = status
			renderer.update(status.RunningInstances, status.PendingInstances)

			if status.Complete {
				renderer.finish(true, "")
				return nil
			}

			// Stalled is terminal — the reconciler has stopped retrying.
			// Waiting longer cannot help; surface the reason now.
			if status.StalledInstances > 0 {
				renderer.finish(false, fmt.Sprintf("%d stalled", status.StalledInstances))
				return scalingDetachError("stalled instance(s)", targetScale, status)
			}

			// Track signature for the time-based detach heuristics.
			sig := fmt.Sprintf("r=%d p=%d f=%d t=%d s=%d",
				status.RunningInstances,
				status.PendingInstances,
				status.FailedInstances,
				status.TerminatingInstances,
				status.CurrentScale)
			if sig != lastSignature {
				lastSignature = sig
				lastChangeAt = time.Now()
			}

			// Drain stuck on Terminating: all that's left is Terminating
			// instances and the signature has been stable for a while.
			// Without this, Ctrl+C is the only way out — the server's
			// completion check waits for them to leave Terminating.
			if targetScale == 0 &&
				status.RunningInstances == 0 &&
				status.PendingInstances == 0 &&
				status.TerminatingInstances > 0 &&
				time.Since(lastChangeAt) > stuckTerminatingTimeout {
				renderer.finish(false, "stuck terminating")
				return scalingDetachError("instances stuck in Terminating", targetScale, status)
			}

			// Opt-in: bail when there's a Failed instance and nothing
			// has changed for a while. Safer than a hard time-based
			// detach because it requires actual evidence of trouble.
			if opts.noProgressTimeout > 0 &&
				status.FailedInstances > 0 &&
				time.Since(lastChangeAt) > opts.noProgressTimeout {
				renderer.finish(false, "no progress")
				return scalingDetachError("no progress while instances are failing", targetScale, status)
			}
		}
	}
}

// scalingDetachError builds a single human-readable error that bundles the
// detach reason with the bounded problem list and a `rune describe` hint.
// Callers print this verbatim, so it owns its own multi-line formatting.
func scalingDetachError(reason string, targetScale int, status *generated.ScalingStatusResponse) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (target scale %d", reason, targetScale)
	if status != nil {
		fmt.Fprintf(&b, "; running=%d pending=%d failed=%d stalled=%d terminating=%d",
			status.RunningInstances,
			status.PendingInstances,
			status.FailedInstances,
			status.StalledInstances,
			status.TerminatingInstances)
	}
	b.WriteString(")")

	if status != nil && len(status.Problems) > 0 {
		b.WriteString("\nProblem instances:")
		for _, p := range status.Problems {
			fmt.Fprintf(&b, "\n  - %s [%s]", p.Name, p.Status)
			if p.Reason != "" {
				fmt.Fprintf(&b, " %s", p.Reason)
			}
			if p.RestartCount > 0 {
				fmt.Fprintf(&b, ", restarts=%d", p.RestartCount)
			}
			if p.Message != "" {
				fmt.Fprintf(&b, " — %s", p.Message)
			}
		}
		b.WriteString("\nHint: `rune describe instance <name>` for full status")
	}

	return errors.New(b.String())
}
