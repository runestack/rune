package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/process"
	"github.com/runestack/rune/pkg/types"
)

func main() {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Create a logger
	logger := log.NewLogger(
		log.WithLevel(log.InfoLevel),
		log.WithFormatter(&log.TextFormatter{
			ShortTimestamp: false,
			DisableColors:  false,
		}),
	)

	// Create process runner
	processRunner, err := process.NewProcessRunner(
		process.WithLogger(logger),
	)
	if err != nil {
		logger.Error("Failed to create process runner", log.Err(err))
		os.Exit(1)
	}

	logger.Info("Created process runner")

	// Create temporary directory for test executable
	tempDir, err := os.MkdirTemp("", "process-runner-example")
	if err != nil {
		logger.Error("Failed to create temp directory", log.Err(err))
		os.Exit(1)
	}
	defer os.RemoveAll(tempDir)

	// Create a test script
	scriptPath := filepath.Join(tempDir, "test-script.sh")
	scriptContent := `#!/bin/sh
echo "Hello from a custom script!"
echo "Current working directory: $(pwd)"
echo "Running as user: $(id -un)"
echo "Arguments: $@"
`
	if err := os.WriteFile(scriptPath, []byte(scriptContent), 0755); err != nil {
		logger.Error("Failed to create test script", log.Err(err))
		os.Exit(1)
	}

	// ------------------------------------------------------------
	// Example 1: Using command from PATH
	// ------------------------------------------------------------
	logger.Info("Example 1: Using command from PATH")

	instance1 := &types.Instance{
		ID:          "path-example-1",
		Name:        "echo-from-path",
		NodeID:      "local",
		ServiceID:   "example-service-id",
		ServiceName: "example-service-name",
		Process: &types.ProcessSpec{
			Command: "echo", // Using command from PATH
			Args:    []string{"This command is found in the system PATH"},
		},
	}

	// Create and run the instance
	runInstance(ctx, processRunner, instance1, logger)

	// ------------------------------------------------------------
	// Example 2: Using absolute path to executable
	// ------------------------------------------------------------
	logger.Info("Example 2: Using absolute path to executable")

	instance2 := &types.Instance{
		ID:          "path-example-2",
		Name:        "script-with-absolute-path",
		NodeID:      "local",
		ServiceID:   "example-service-id",
		ServiceName: "example-service-name",
		Process: &types.ProcessSpec{
			Command: scriptPath, // Using absolute path to our test script
			Args:    []string{"arg1", "arg2", "arg3"},
		},
	}

	// Create and run the instance
	runInstance(ctx, processRunner, instance2, logger)

	// ------------------------------------------------------------
	// Example 3: With Security Context (if not running as root, this may fail)
	// ------------------------------------------------------------
	logger.Info("Example 3: With Security Context (may require root)")

	// Create instance with security context
	// Note: This example will only work properly when running as root
	instance3 := &types.Instance{
		ID:          "path-example-3",
		Name:        "with-security-context",
		NodeID:      "local",
		ServiceID:   "example-service-id",
		ServiceName: "example-service-name",
		Process: &types.ProcessSpec{
			Command: scriptPath,
			Args:    []string{"with security context"},
			SecurityContext: &types.ProcessSecurityContext{
				// On most systems, 'nobody' is a restricted user that exists
				// Note: This requires the example to be run as root
				User:       "nobody",
				ReadOnlyFS: true, // This is currently just logged as a warning
			},
		},
	}

	// Try to create and run the instance, but handle failure gracefully
	err = processRunner.Create(ctx, instance3)
	if err != nil {
		logger.Warn("Could not create instance with security context",
			log.Str("error", err.Error()),
			log.Str("note", "This is expected if not running as root"))
	} else {
		// If creation succeeded, try to run it
		// Don't re-create the instance, just start, get logs, and clean up
		if err := processRunner.Start(ctx, instance3); err != nil {
			logger.Error("Failed to start instance with security context",
				log.Str("id", instance3.ID),
				log.Err(err))
			// Clean up
			_ = processRunner.Remove(ctx, instance3, true)
		} else {
			// Wait for process to complete
			time.Sleep(500 * time.Millisecond)

			// Get logs
			logs, err := processRunner.GetLogs(ctx, instance3, runner.LogOptions{})
			if err != nil {
				logger.Error("Failed to get logs",
					log.Str("id", instance3.ID),
					log.Err(err))
			} else {
				// Read and print logs
				logContent, err := io.ReadAll(logs)
				logs.Close()
				if err != nil {
					logger.Error("Failed to read logs",
						log.Str("id", instance3.ID),
						log.Err(err))
				} else {
					fmt.Printf("--- Logs for %s ---\n%s\n", instance3.ID, string(logContent))
				}
			}

			// Cleanup
			if err := processRunner.Remove(ctx, instance3, true); err != nil {
				logger.Error("Failed to remove instance",
					log.Str("id", instance3.ID),
					log.Err(err))
			}
		}
	}

	// ------------------------------------------------------------
	// Example 4: Invalid path (should fail with validation error)
	// ------------------------------------------------------------
	logger.Info("Example 4: Invalid path (should fail with validation error)")

	instance4 := &types.Instance{
		ID:          "path-example-4",
		Name:        "invalid-path",
		NodeID:      "local",
		ServiceID:   "example-service-id",
		ServiceName: "example-service-name",
		Process: &types.ProcessSpec{
			Command: "/path/to/nonexistent/executable",
			Args:    []string{},
		},
	}

	// Try to create (should fail with validation error)
	err = processRunner.Create(ctx, instance4)
	if err != nil {
		logger.Info("Received expected validation error",
			log.Str("error", err.Error()))
	} else {
		logger.Error("Expected validation to fail but it did not")
	}

	// ------------------------------------------------------------
	// Example 5: Non-executable file (should fail with validation error)
	// ------------------------------------------------------------
	logger.Info("Example 5: Non-executable file (should fail)")

	// Create a non-executable file
	nonExecPath := filepath.Join(tempDir, "non-executable.txt")
	if err := os.WriteFile(nonExecPath, []byte("This is not executable"), 0644); err != nil {
		logger.Error("Failed to create non-executable file", log.Err(err))
	} else {
		instance5 := &types.Instance{
			ID:          "path-example-5",
			Name:        "non-executable-file",
			NodeID:      "local",
			ServiceID:   "example-service-id",
			ServiceName: "example-service-name",
			Process: &types.ProcessSpec{
				Command: nonExecPath,
				Args:    []string{},
			},
		}

		// Try to create (should fail with validation error)
		err = processRunner.Create(ctx, instance5)
		if err != nil {
			logger.Info("Received expected validation error",
				log.Str("error", err.Error()))
		} else {
			logger.Error("Expected validation to fail but it did not")
		}
	}

	logger.Info("Process runner path validation example completed")
}

// runInstance is a helper function to create, start, get logs, and cleanup an instance
func runInstance(ctx context.Context, processRunner *process.ProcessRunner, instance *types.Instance, logger log.Logger) {
	// Create the instance
	if err := processRunner.Create(ctx, instance); err != nil {
		logger.Error("Failed to create instance",
			log.Str("id", instance.ID),
			log.Err(err))
		return
	}

	// Start the instance
	if err := processRunner.Start(ctx, instance); err != nil {
		logger.Error("Failed to start instance",
			log.Str("id", instance.ID),
			log.Err(err))
		// Clean up
		_ = processRunner.Remove(ctx, instance, true)
		return
	}

	// Wait for process to complete
	time.Sleep(500 * time.Millisecond)

	// Get logs
	logs, err := processRunner.GetLogs(ctx, instance, runner.LogOptions{})
	if err != nil {
		logger.Error("Failed to get logs",
			log.Str("id", instance.ID),
			log.Err(err))
	} else {
		// Read and print logs
		logContent, err := io.ReadAll(logs)
		logs.Close()
		if err != nil {
			logger.Error("Failed to read logs",
				log.Str("id", instance.ID),
				log.Err(err))
		} else {
			fmt.Printf("--- Logs for %s ---\n%s\n", instance.ID, string(logContent))
		}
	}

	// Cleanup
	if err := processRunner.Remove(ctx, instance, true); err != nil {
		logger.Error("Failed to remove instance",
			log.Str("id", instance.ID),
			log.Err(err))
	}
}
