package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/process"
	"github.com/runestack/rune/pkg/types"
)

// This example demonstrates a longer-running process with interactive management
func main() {
	// Create a context with a timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Create a logger with the text formatter
	logger := log.NewLogger(
		log.WithLevel(log.InfoLevel),
		log.WithFormatter(&log.TextFormatter{
			ShortTimestamp: false, // Use full timestamp (default)
			DisableColors:  false, // Enable colors
		}),
	)

	// Create a process runner
	processRunner, err := process.NewProcessRunner(
		process.WithLogger(logger),
	)
	if err != nil {
		logger.Error("Failed to create process runner", log.Err(err))
		os.Exit(1)
	}

	logger.Info("Created process runner")

	// Create an instance running a simple interval counter script
	instance := &types.Instance{
		ID:          "counter-process",
		Name:        "counter",
		NodeID:      "local",
		ServiceID:   "example-service-id",
		ServiceName: "example-service-name",
		Process: &types.ProcessSpec{
			// Run a shell script that counts to 30 with 1-second intervals
			Command: "sh",
			Args: []string{
				"-c",
				"for i in $(seq 1 30); do echo \"Count: $i\"; sleep 1; done",
			},
		},
	}

	// Create the instance
	logger.Info("Creating counter process instance")
	if err := processRunner.Create(ctx, instance); err != nil {
		logger.Error("Failed to create instance", log.Err(err))
		os.Exit(1)
	}

	// Start the instance
	logger.Info("Starting counter process")
	if err := processRunner.Start(ctx, instance); err != nil {
		logger.Error("Failed to start instance", log.Err(err))
		os.Exit(1)
	}

	// Wait a bit to let it start
	time.Sleep(2 * time.Second)

	// Get initial status
	status, err := processRunner.Status(ctx, instance)
	if err != nil {
		logger.Error("Failed to get status", log.Err(err))
		os.Exit(1)
	}
	logger.Info("Process status", log.Str("status", string(status)))

	// Follow logs with timeout
	logger.Info("Following logs for 5 seconds...")
	logCtx, logCancel := context.WithTimeout(ctx, 5*time.Second)
	defer logCancel()

	logs, err := processRunner.GetLogs(logCtx, instance, runner.LogOptions{
		Follow:     true,
		Timestamps: true,
	})
	if err != nil {
		logger.Error("Failed to get logs", log.Err(err))
		os.Exit(1)
	}

	// Copy logs to stdout until context timeout
	fmt.Println("Log stream:")
	_, err = io.Copy(os.Stdout, logs)
	if err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		logger.Error("Error reading logs", log.Err(err))
	}
	logs.Close()
	fmt.Println("\nStopped following logs")

	// Pause for user to see output
	logger.Info("Process is still running, checking status...")
	time.Sleep(1 * time.Second)

	// Get current status
	status, err = processRunner.Status(ctx, instance)
	if err != nil {
		logger.Error("Failed to get status", log.Err(err))
		os.Exit(1)
	}
	logger.Info("Current process status", log.Str("status", string(status)))

	// Gracefully stop the process
	logger.Info("Stopping process...")
	if err := processRunner.Stop(ctx, instance, 5*time.Second); err != nil {
		logger.Error("Failed to stop process", log.Err(err))
		os.Exit(1)
	}

	// Check status after stopping
	status, err = processRunner.Status(ctx, instance)
	if err != nil {
		logger.Error("Failed to get status", log.Err(err))
		os.Exit(1)
	}
	logger.Info("Status after stopping", log.Str("status", string(status)))

	// Get final logs
	finalLogs, err := processRunner.GetLogs(ctx, instance, runner.LogOptions{
		Tail: 5, // Just the last 5 lines
	})
	if err != nil {
		logger.Error("Failed to get final logs", log.Err(err))
		os.Exit(1)
	}
	defer finalLogs.Close()

	// Show final log lines
	fmt.Println("\nFinal log lines:")
	logContent, err := io.ReadAll(finalLogs)
	if err != nil {
		logger.Error("Failed to read logs", log.Err(err))
		os.Exit(1)
	}
	fmt.Println(string(logContent))

	// Remove the instance
	logger.Info("Removing process instance")
	if err := processRunner.Remove(ctx, instance, false); err != nil {
		logger.Error("Failed to remove instance", log.Err(err))
		os.Exit(1)
	}

	logger.Info("Long-running process example completed successfully")
}
