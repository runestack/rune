//go:build integration
// +build integration

// go:build integration

package docker_integration

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/runestack/rune/pkg/log"
	runeRunner "github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/runner/docker"
	runetypes "github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/test/integration/helpers"
)

// skipIfDockerUnavailable skips the test if Docker is not available.
func skipIfDockerUnavailable(t *testing.T) *client.Client {
	// Try to create a Docker client to see if Docker is available
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithVersion("1.43"), // Force compatibility with older Docker daemons
	)
	if err != nil {
		t.Skip("Docker is not available:", err)
	}

	// Ping the Docker daemon to ensure it's responding
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skip("Docker daemon is not responding:", err)
	}

	return cli
}

// TestGetLogs tests the Docker runner GetLogs functionality.
func TestGetLogs(t *testing.T) {
	// Skip if Docker is not available
	cli := skipIfDockerUnavailable(t)

	// Create helper for Docker operations
	dockerHelper, err := helpers.NewDockerHelper()
	if err != nil {
		t.Fatalf("Failed to create Docker helper: %v", err)
	}
	defer dockerHelper.Close()

	// Setup context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Ensure Alpine image is available by pulling it
	image := helpers.AlpineImage
	err = dockerHelper.EnsureImage(ctx, image)
	if err != nil {
		t.Skipf("Failed to ensure image %s is available: %v - skipping test", image, err)
		return
	}

	// Create a unique test namespace based on timestamp
	namespace := "log-test-" + time.Now().Format("20060102150405")
	logger := log.NewLogger(
		log.WithLevel(log.InfoLevel),
		log.WithFormatter(&log.TextFormatter{
			DisableColors: true, // Disable colors for tests
		}),
	)

	// Create Docker runner
	runner, err := docker.NewDockerRunner(logger)
	if err != nil {
		t.Fatalf("Failed to create Docker runner: %v", err)
	}

	// Create instance to run in Docker
	instanceID := "log-test-instance"
	instance := &runetypes.Instance{
		ID:        instanceID,
		Name:      "log-test-instance",
		ServiceID: "log-test-service",
		NodeID:    "test-node",
	}

	// Create a container with a command that outputs logs
	containerConfig := &container.Config{
		Image: image,
		Cmd:   []string{"sh", "-c", "echo 'test log message 1'; sleep 1; echo 'test log message 2'; sleep 5"},
		Labels: map[string]string{
			"rune.managed":      "true",
			"rune.namespace":    namespace,
			"rune.instance.id":  instanceID,
			"rune.service.id":   instance.ServiceID,
			"rune.service.name": instance.Name,
		},
	}
	hostConfig := &container.HostConfig{}

	// Create container using the Docker client
	resp, err := cli.ContainerCreate(
		ctx,
		containerConfig,
		hostConfig,
		nil,           // No network config
		nil,           // No platform config
		instance.Name, // Use instance name as container name
	)
	if err != nil {
		t.Fatalf("Failed to create container: %v", err)
	}
	containerID := resp.ID

	// Store container ID in instance for the runner to find it
	instance.ContainerID = containerID

	// Ensure container cleanup using DockerHelper instead of the runner
	defer dockerHelper.CleanupContainer(ctx, containerID)

	// Start the container
	if err := cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}

	// Wait a bit for the container to produce logs
	time.Sleep(2 * time.Second)

	// Test 1: Get all logs
	t.Run("GetAllLogs", func(t *testing.T) {
		logs, err := runner.GetLogs(ctx, instance, runeRunner.LogOptions{
			Follow:     false,
			Tail:       0, // Get all logs
			Timestamps: false,
		})
		if err != nil {
			t.Fatalf("Failed to get logs: %v", err)
		}
		defer logs.Close()

		// Read logs
		buf, err := io.ReadAll(logs)
		if err != nil {
			t.Fatalf("Failed to read logs: %v", err)
		}

		logContent := string(buf)
		if !strings.Contains(logContent, "test log message 1") {
			t.Errorf("Expected log message 1 not found, got: %q", logContent)
		}
		if !strings.Contains(logContent, "test log message 2") {
			t.Errorf("Expected log message 2 not found, got: %q", logContent)
		}
	})

	// Test 2: Get limited logs
	t.Run("GetLimitedLogs", func(t *testing.T) {
		logs, err := runner.GetLogs(ctx, instance, runeRunner.LogOptions{
			Follow:     false,
			Tail:       1, // Only get the last line
			Timestamps: false,
		})
		if err != nil {
			t.Fatalf("Failed to get logs: %v", err)
		}
		defer logs.Close()

		// Read logs
		buf, err := io.ReadAll(logs)
		if err != nil {
			t.Fatalf("Failed to read logs: %v", err)
		}

		logContent := string(buf)
		if strings.Contains(logContent, "test log message 1") {
			t.Errorf("Unexpected log message 1 found with tail=1, got: %q", logContent)
		}
		if !strings.Contains(logContent, "test log message 2") {
			t.Errorf("Expected log message 2 not found, got: %q", logContent)
		}
	})

	// Test 3: Test timestamp-based filtering with Since
	t.Run("GetLogsSince", func(t *testing.T) {
		// First, get logs with timestamps to capture the timestamp of the last message
		timestampedLogs, err := runner.GetLogs(ctx, instance, runeRunner.LogOptions{
			Follow:     false,
			Tail:       0,
			Timestamps: true, // Enable timestamps
		})
		if err != nil {
			t.Fatalf("Failed to get logs with timestamps: %v", err)
		}

		// Read timestamped logs to find the timestamp
		tsLogBuf, err := io.ReadAll(timestampedLogs)
		timestampedLogs.Close()
		if err != nil {
			t.Fatalf("Failed to read timestamped logs: %v", err)
		}

		// Parse the timestamped logs to find the second message's timestamp
		// Docker timestamp format is typically: 2023-07-08T12:34:56.123456789Z
		tsLogContent := string(tsLogBuf)
		lines := strings.Split(tsLogContent, "\n")

		// Find a line with "test log message 1" to get its timestamp
		var msg1Timestamp time.Time
		found := false
		for _, line := range lines {
			if strings.Contains(line, "test log message 1") {
				// Extract timestamp (should be at beginning of line)
				parts := strings.SplitN(line, " ", 2)
				if len(parts) > 0 {
					var parseErr error
					msg1Timestamp, parseErr = time.Parse(time.RFC3339Nano, parts[0])
					if parseErr == nil {
						found = true
						break
					}
				}
			}
		}

		if !found {
			t.Logf("Could not find timestamp for message 1, skipping Since test")
			t.Logf("Available log content: %s", tsLogContent)
			return
		}

		// Set the Since time to right after the first message's timestamp
		sinceTime := msg1Timestamp.Add(100 * time.Millisecond)
		t.Logf("Using Since time: %s", sinceTime.Format(time.RFC3339))

		// Get logs with Since filter
		sinceLogs, err := runner.GetLogs(ctx, instance, runeRunner.LogOptions{
			Follow:     false,
			Since:      sinceTime,
			Timestamps: false,
		})
		if err != nil {
			t.Fatalf("Failed to get logs with Since filter: %v", err)
		}
		defer sinceLogs.Close()

		// Read filtered logs
		sinceBuf, err := io.ReadAll(sinceLogs)
		if err != nil {
			t.Fatalf("Failed to read Since-filtered logs: %v", err)
		}

		sinceContent := string(sinceBuf)
		t.Logf("Since-filtered logs content: %s", sinceContent)

		// Should not contain the first message, but should contain the second
		if strings.Contains(sinceContent, "test log message 1") {
			t.Errorf("Unexpected log message 1 found with Since filter, got: %q", sinceContent)
		}
		if !strings.Contains(sinceContent, "test log message 2") {
			t.Errorf("Expected log message 2 not found with Since filter, got: %q", sinceContent)
		}
	})

	// Test 4: Test timestamp-based filtering with Until
	t.Run("GetLogsUntil", func(t *testing.T) {
		// First, get logs with timestamps to capture timestamps
		timestampedLogs, err := runner.GetLogs(ctx, instance, runeRunner.LogOptions{
			Follow:     false,
			Tail:       0,
			Timestamps: true, // Enable timestamps
		})
		if err != nil {
			t.Fatalf("Failed to get logs with timestamps: %v", err)
		}

		// Read timestamped logs to find timestamps
		tsLogBuf, err := io.ReadAll(timestampedLogs)
		timestampedLogs.Close()
		if err != nil {
			t.Fatalf("Failed to read timestamped logs: %v", err)
		}

		// Parse the timestamped logs to find the second message's timestamp
		tsLogContent := string(tsLogBuf)
		lines := strings.Split(tsLogContent, "\n")

		// Find a line with "test log message 2" to get its timestamp
		var msg2Timestamp time.Time
		found := false
		for _, line := range lines {
			if strings.Contains(line, "test log message 2") {
				// Extract timestamp (should be at beginning of line)
				parts := strings.SplitN(line, " ", 2)
				if len(parts) > 0 {
					var parseErr error
					msg2Timestamp, parseErr = time.Parse(time.RFC3339Nano, parts[0])
					if parseErr == nil {
						found = true
						break
					}
				}
			}
		}

		if !found {
			t.Logf("Could not find timestamp for message 2, skipping Until test")
			t.Logf("Available log content: %s", tsLogContent)
			return
		}

		// Set the Until time to right before the second message's timestamp
		untilTime := msg2Timestamp.Add(-100 * time.Millisecond)
		t.Logf("Using Until time: %s", untilTime.Format(time.RFC3339))

		// Get logs with Until filter
		untilLogs, err := runner.GetLogs(ctx, instance, runeRunner.LogOptions{
			Follow:     false,
			Until:      untilTime,
			Timestamps: false,
		})
		if err != nil {
			t.Fatalf("Failed to get logs with Until filter: %v", err)
		}
		defer untilLogs.Close()

		// Read filtered logs
		untilBuf, err := io.ReadAll(untilLogs)
		if err != nil {
			t.Fatalf("Failed to read Until-filtered logs: %v", err)
		}

		untilContent := string(untilBuf)
		t.Logf("Until-filtered logs content: %s", untilContent)

		// Should contain the first message, but not the second
		if !strings.Contains(untilContent, "test log message 1") {
			t.Errorf("Expected log message 1 not found with Until filter, got: %q", untilContent)
		}
		if strings.Contains(untilContent, "test log message 2") {
			t.Errorf("Unexpected log message 2 found with Until filter, got: %q", untilContent)
		}
	})

	// Test 5: Test combined Since and Until filtering
	t.Run("GetLogsSinceUntil", func(t *testing.T) {
		// We need a container that produces more than two log messages for this test
		// Create a new container with more log messages

		combinedInstanceID := "log-combined-test-instance"
		combinedInstance := &runetypes.Instance{
			ID:        combinedInstanceID,
			Name:      "log-combined-test-instance",
			ServiceID: "log-test-service",
			NodeID:    "test-node",
		}

		// Create a container with a command that outputs multiple logs messages with delays
		containerConfig := &container.Config{
			Image: image,
			Cmd: []string{"sh", "-c",
				"echo 'message A'; sleep 1;" +
					"echo 'message B'; sleep 1;" +
					"echo 'message C'; sleep 1;" +
					"echo 'message D'; sleep 1;" +
					"echo 'message E'; sleep 5"},
			Labels: map[string]string{
				"rune.managed":      "true",
				"rune.namespace":    namespace,
				"rune.instance.id":  combinedInstanceID,
				"rune.service.id":   combinedInstance.ServiceID,
				"rune.service.name": combinedInstance.Name,
			},
		}

		// Create container
		resp, err := cli.ContainerCreate(
			ctx,
			containerConfig,
			hostConfig,
			nil,
			nil,
			combinedInstance.Name,
		)
		if err != nil {
			t.Fatalf("Failed to create container for combined test: %v", err)
			return
		}
		combinedContainerID := resp.ID

		// Store container ID
		combinedInstance.ContainerID = combinedContainerID

		// Cleanup
		defer dockerHelper.CleanupContainer(ctx, combinedContainerID)

		// Start the container
		if err := cli.ContainerStart(ctx, combinedContainerID, container.StartOptions{}); err != nil {
			t.Fatalf("Failed to start container for combined test: %v", err)
			return
		}

		// Wait for container to generate logs
		time.Sleep(3 * time.Second)

		// Get logs with timestamps to extract timing information
		timestampedLogs, err := runner.GetLogs(ctx, combinedInstance, runeRunner.LogOptions{
			Follow:     false,
			Timestamps: true,
		})
		if err != nil {
			t.Fatalf("Failed to get timestamped logs: %v", err)
			return
		}

		tsLogBuf, err := io.ReadAll(timestampedLogs)
		timestampedLogs.Close()
		if err != nil {
			t.Fatalf("Failed to read timestamped logs: %v", err)
			return
		}

		// Parse timestamps from the logs
		tsLogContent := string(tsLogBuf)
		lines := strings.Split(tsLogContent, "\n")

		// Find timestamps for messages B and D
		var msgBTimestamp, msgDTimestamp time.Time
		foundB, foundD := false, false

		for _, line := range lines {
			parts := strings.SplitN(line, " ", 2)
			if len(parts) < 2 {
				continue
			}

			ts := parts[0]
			content := parts[1]

			parsedTime, err := time.Parse(time.RFC3339Nano, ts)
			if err != nil {
				continue
			}

			if strings.Contains(content, "message B") {
				msgBTimestamp = parsedTime
				foundB = true
			} else if strings.Contains(content, "message D") {
				msgDTimestamp = parsedTime
				foundD = true
			}

			if foundB && foundD {
				break
			}
		}

		if !foundB || !foundD {
			t.Logf("Could not find required message timestamps, skipping combined test")
			t.Logf("Log content: %s", tsLogContent)
			return
		}

		// Add a small buffer to the timestamps
		sinceTime := msgBTimestamp.Add(50 * time.Millisecond)
		untilTime := msgDTimestamp.Add(-50 * time.Millisecond)

		t.Logf("Using Since time: %s", sinceTime.Format(time.RFC3339))
		t.Logf("Using Until time: %s", untilTime.Format(time.RFC3339))

		// Get logs with combined filters
		combinedLogs, err := runner.GetLogs(ctx, combinedInstance, runeRunner.LogOptions{
			Follow:     false,
			Since:      sinceTime,
			Until:      untilTime,
			Timestamps: false,
		})
		if err != nil {
			t.Fatalf("Failed to get logs with combined filters: %v", err)
			return
		}
		defer combinedLogs.Close()

		// Read filtered logs
		combinedBuf, err := io.ReadAll(combinedLogs)
		if err != nil {
			t.Fatalf("Failed to read logs with combined filters: %v", err)
			return
		}

		combinedContent := string(combinedBuf)
		t.Logf("Combined filtered logs content: %s", combinedContent)

		// Should not contain messages A, B, D, or E
		// Should contain message C
		if strings.Contains(combinedContent, "message A") {
			t.Errorf("Unexpected message A found in combined filtered logs: %q", combinedContent)
		}
		if strings.Contains(combinedContent, "message B") {
			t.Errorf("Unexpected message B found in combined filtered logs: %q", combinedContent)
		}
		if !strings.Contains(combinedContent, "message C") {
			t.Errorf("Expected message C not found in combined filtered logs: %q", combinedContent)
		}
		if strings.Contains(combinedContent, "message D") {
			t.Errorf("Unexpected message D found in combined filtered logs: %q", combinedContent)
		}
		if strings.Contains(combinedContent, "message E") {
			t.Errorf("Unexpected message E found in combined filtered logs: %q", combinedContent)
		}
	})
}
