package process_test

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

func Example() {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create a logger
	logger := log.NewLogger()

	// Create a process runner with options
	processRunner, err := process.NewProcessRunner(
		process.WithBaseDir(os.TempDir()),
		process.WithLogger(logger),
	)
	if err != nil {
		fmt.Printf("Failed to create process runner: %v\n", err)
		return
	}

	// Create a unique instance ID
	instanceID := "example-process-123"

	// Create a process instance that will list files
	instance := &types.Instance{
		ID:     instanceID,
		Name:   "example-process",
		NodeID: "local",
		Process: &types.ProcessSpec{
			Command: "ls",
			Args:    []string{"-la"},
		},
	}

	// Create the instance
	fmt.Println("Creating process instance...")
	if err := processRunner.Create(ctx, instance); err != nil {
		fmt.Printf("Failed to create instance: %v\n", err)
		return
	}

	// Start the instance
	fmt.Println("Starting process instance...")
	if err := processRunner.Start(ctx, instance); err != nil {
		fmt.Printf("Failed to start instance: %v\n", err)
		return
	}

	// Give the process time to complete
	time.Sleep(1 * time.Second)

	// Get the status
	status, err := processRunner.Status(ctx, instance)
	if err != nil {
		fmt.Printf("Failed to get status: %v\n", err)
		return
	}
	fmt.Printf("Instance status: %s\n", status)

	// Get logs
	fmt.Println("Getting logs...")
	logs, err := processRunner.GetLogs(ctx, instance, runner.LogOptions{
		Tail: 10,
	})
	if err != nil {
		fmt.Printf("Failed to get logs: %v\n", err)
		return
	}
	defer logs.Close()

	// Print logs content to stdout
	logBytes, err := io.ReadAll(logs)
	if err != nil {
		fmt.Printf("Failed to read logs: %v\n", err)
		return
	}
	fmt.Printf("Log output: %s\n", string(logBytes))

	// List all instances
	instances, err := processRunner.List(ctx, "example")
	if err != nil {
		fmt.Printf("Failed to list instances: %v\n", err)
		return
	}
	fmt.Printf("Found %d instances\n", len(instances))

	// Remove the instance
	fmt.Println("Removing process instance...")
	if err := processRunner.Remove(ctx, instance, true); err != nil {
		fmt.Printf("Failed to remove instance: %v\n", err)
		return
	}

	fmt.Println("Process runner example completed successfully")
}
