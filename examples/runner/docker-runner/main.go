package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/docker"
	"github.com/runestack/rune/pkg/types"
)

func main() {
	// Create a logger with the text formatter
	logger := log.NewLogger(
		log.WithLevel(log.InfoLevel),
		log.WithFormatter(&log.TextFormatter{
			ShortTimestamp: false, // Use full timestamp (default)
			DisableColors:  false, // Enable colors
		}),
	)

	// Set it as the default logger
	log.SetDefaultLogger(logger)

	// Create a component-specific logger
	runnerLogger := logger.WithComponent("runner-example")
	runnerLogger.Info("Starting Rune Docker Runner example")

	// Create a Docker runner
	dockerRunner, err := docker.NewDockerRunner(runnerLogger)
	if err != nil {
		runnerLogger.Fatal("Failed to create Docker runner", log.Err(err))
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Create a unique ID for the instance
	instanceID := uuid.New().String()
	instanceName := fmt.Sprintf("example-%s", instanceID[:8])

	// Create instance object
	instance := &types.Instance{
		ID:          instanceID,
		Name:        instanceName,
		ServiceID:   "example-service",
		ServiceName: "example-service",
		NodeID:      "local",
	}

	// Create the instance
	runnerLogger.Info("Creating instance", log.Str("name", instanceName))
	if err := dockerRunner.Create(ctx, instance); err != nil {
		runnerLogger.Fatal("Failed to create instance", log.Err(err))
	}
	runnerLogger.Info("Created instance",
		log.Str("container_id", instance.ContainerID),
		log.Str("instance_id", instance.ID))

	// Start the instance
	runnerLogger.Info("Starting instance", log.Str("id", instanceID))
	if err := dockerRunner.Start(ctx, instance); err != nil {
		runnerLogger.Fatal("Failed to start instance", log.Err(err))
	}

	// Wait a bit for the container to start up
	runnerLogger.Info("Waiting for container to start...")
	time.Sleep(2 * time.Second)

	// Get the status
	status, err := dockerRunner.Status(ctx, instance)
	if err != nil {
		runnerLogger.Fatal("Failed to get status", log.Err(err))
	}
	runnerLogger.Info("Instance status", log.Str("status", string(status)))

	// Get logs
	runnerLogger.Info("Getting logs for instance", log.Str("id", instanceID))
	logs, err := dockerRunner.GetLogs(ctx, instance, runner.LogOptions{
		Tail:       10,
		Follow:     false,
		Timestamps: true,
	})
	if err != nil {
		runnerLogger.Fatal("Failed to get logs", log.Err(err))
	}

	// Print logs
	runnerLogger.Info("Logs:")
	if _, err := io.Copy(os.Stdout, logs); err != nil {
		runnerLogger.Fatal("Failed to read logs", log.Err(err))
	}
	logs.Close()

	// List all instances
	runnerLogger.Info("Listing all instances:")
	instances, err := dockerRunner.List(ctx, instance.Namespace)
	if err != nil {
		runnerLogger.Fatal("Failed to list instances", log.Err(err))
	}
	for _, inst := range instances {
		runnerLogger.Info("Instance",
			log.Str("name", inst.Name),
			log.Str("id", inst.ID),
			log.Str("status", string(inst.Status)))
	}

	// Cleanup
	runnerLogger.Info("Stopping instance", log.Str("id", instanceID))
	if err := dockerRunner.Stop(ctx, instance, 10*time.Second); err != nil {
		runnerLogger.Fatal("Failed to stop instance", log.Err(err))
	}

	runnerLogger.Info("Removing instance", log.Str("id", instanceID))
	if err := dockerRunner.Remove(ctx, instance, false); err != nil {
		runnerLogger.Fatal("Failed to remove instance", log.Err(err))
	}

	runnerLogger.Info("Example completed successfully!")
}
