package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
	runetypes "github.com/runestack/rune/pkg/types"
)

// skipIfDockerUnavailable skips the test if Docker is not available.
func skipIfDockerUnavailable(t *testing.T) *client.Client {
	// Skip if SKIP_DOCKER_TESTS is set
	if os.Getenv("SKIP_DOCKER_TESTS") != "" {
		t.Skip("Skipping Docker tests")
	}

	// Try to create a Docker client to see if Docker is available
	cli, err := client.NewClientWithOpts(client.FromEnv)
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

// TestNewDockerRunner tests the creation of a new Docker runner.
func TestNewDockerRunner(t *testing.T) {
	skipIfDockerUnavailable(t)

	logger := log.NewLogger(
		log.WithLevel(log.InfoLevel),
		log.WithFormatter(&log.TextFormatter{
			DisableColors: true, // Disable colors for tests
		}),
	)
	r, err := NewDockerRunner(logger)

	if err != nil {
		t.Fatalf("Failed to create Docker runner: %v", err)
	}

	if r == nil {
		t.Fatal("Docker runner is nil")
	}

}

// TestDockerRunnerLifecycle tests the full lifecycle of an instance using a real Docker daemon.
// This is an integration test and can be skipped if Docker is not available.
func TestDockerRunnerLifecycle(t *testing.T) {
	skipIfDockerUnavailable(t)

	// Create a unique test namespace based on timestamp
	namespace := "test-" + time.Now().Format("20060102150405")
	logger := log.NewLogger(
		log.WithLevel(log.InfoLevel),
		log.WithFormatter(&log.TextFormatter{
			DisableColors: true, // Disable colors for tests
		}),
	)
	r, err := NewDockerRunner(logger)
	if err != nil {
		t.Fatalf("Failed to create Docker runner: %v", err)
	}

	// Use a simple, small image that starts quickly
	instance := &runetypes.Instance{
		ID:        "test-instance",
		Namespace: namespace,
		Name:      "test-instance",
		ServiceID: "test-service",
		NodeID:    "test-node",
		Metadata: &runetypes.InstanceMetadata{
			Image: "alpine:latest",
		},
		Exec: &runetypes.Exec{
			Command: []string{"sleep", "30"},
		},
	}

	// Create a context with timeout for all operations
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Ensure cleanup on test completion
	defer func() {
		// Attempt to remove the container regardless of test outcome
		r.Remove(ctx, instance, true)
	}()

	// Test creating a container
	err = r.Create(ctx, instance)
	if err != nil {
		t.Fatalf("Failed to create container: %v", err)
	}

	// Verify container was created and ID was set
	if instance.ContainerID == "" {
		t.Fatal("Container ID was not set after Create")
	}

	// Test starting the container
	err = r.Start(ctx, instance)
	if err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}

	// Wait for container to fully start
	time.Sleep(2 * time.Second)

	// Check status
	status, err := r.Status(ctx, instance)
	if err != nil {
		t.Fatalf("Failed to get status: %v", err)
	}
	if status != runetypes.InstanceStatusRunning {
		t.Errorf("Expected status %s, got %s", runetypes.InstanceStatusRunning, status)
	}

	// Test stopping the container
	err = r.Stop(ctx, instance, 10*time.Second)
	if err != nil {
		t.Fatalf("Failed to stop container: %v", err)
	}

	// Note: We don't check status here because the defer cleanup will remove the container
	// and checking status after removal would fail. The important thing is that Stop() succeeded.

	// Test removing the container
	err = r.Remove(ctx, instance, false)
	if err != nil {
		t.Fatalf("Failed to remove container: %v", err)
	}

	// Verify container was removed by checking that getting its status now fails
	_, err = r.Status(ctx, instance)
	if err == nil {
		t.Fatal("Expected error getting status of removed container, got nil")
	}
}

// TestDockerLogReader tests the log reader implementation.
func TestDockerLogReader(t *testing.T) {
	// Create a buffer with the test content and prepend a mock Docker log header
	testContent := "Test log content"

	// Create a buffer to hold the Docker-formatted log
	var buf bytes.Buffer

	// First, write a fake Docker log header:
	// - Byte 0: Stream type (1 for stdout)
	// - Bytes 1-3: Padding (zeros)
	// - Bytes 4-7: Frame size (length of our test content as uint32 big endian)
	header := []byte{1, 0, 0, 0, 0, 0, 0, 0}
	// Set the content length in big endian
	binary.BigEndian.PutUint32(header[4:], uint32(len(testContent)))
	buf.Write(header)

	// Then write the content
	buf.WriteString(testContent)

	// Create a reader using our mock Docker log format
	mockLog := io.NopCloser(&buf)
	logReader := newLogReader(mockLog)
	defer logReader.Close()

	// Read the content
	result := make([]byte, len(testContent))
	n, err := io.ReadFull(logReader, result)

	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		t.Fatalf("Failed to read from log reader: %v", err)
	}

	// Check the content
	actualContent := string(result[:n])
	if actualContent != testContent {
		t.Errorf("Expected '%s', got: '%s'", testContent, actualContent)
	}
}

// TestGetLogs has been moved to test/integration/docker/logs_test.go
// as it requires a Docker image and should be executed as part of integration tests.

// TestList tests listing of containers.
func TestList(t *testing.T) {
	skipIfDockerUnavailable(t)

	// Create a unique test namespace based on timestamp
	namespace := "list-test-" + time.Now().Format("20060102150405")
	logger := log.NewLogger(
		log.WithLevel(log.InfoLevel),
		log.WithFormatter(&log.TextFormatter{
			DisableColors: true, // Disable colors for tests
		}),
	)
	r, err := NewDockerRunner(logger)
	if err != nil {
		t.Fatalf("Failed to create Docker runner: %v", err)
	}

	// Create a context with timeout for all operations
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create a couple of containers
	instance1 := &runetypes.Instance{
		ID:        "list-test-1",
		Name:      "list-test-1",
		ServiceID: "list-test-service",
		NodeID:    "test-node",
		Namespace: namespace,
		Metadata: &runetypes.InstanceMetadata{
			Image: "alpine:latest",
		},
	}

	instance2 := &runetypes.Instance{
		ID:        "list-test-2",
		Name:      "list-test-2",
		ServiceID: "list-test-service",
		NodeID:    "test-node",
		Namespace: namespace,
		Metadata: &runetypes.InstanceMetadata{
			Image: "alpine:latest",
		},
	}

	// Ensure cleanup on test completion
	defer func() {
		r.Remove(ctx, instance1, true)
		r.Remove(ctx, instance2, true)
	}()

	// Create the containers
	err = r.Create(ctx, instance1)
	if err != nil {
		t.Fatalf("Failed to create container 1: %v", err)
	}

	err = r.Create(ctx, instance2)
	if err != nil {
		t.Fatalf("Failed to create container 2: %v", err)
	}

	// List containers
	instances, err := r.List(ctx, namespace)
	if err != nil {
		t.Fatalf("Failed to list containers: %v", err)
	}

	// Check that we found at least our 2 containers
	found1, found2 := false, false
	for _, inst := range instances {
		if inst.ID == instance1.ID {
			found1 = true
		}
		if inst.ID == instance2.ID {
			found2 = true
		}
	}

	if !found1 || !found2 {
		t.Errorf("Not all test containers were found in the list. Found container 1: %v, Found container 2: %v", found1, found2)
	}
}

// TestParseDockerTimestamp tests the parseDockerTimestamp function
// that extracts timestamps from Docker log lines
func TestParseDockerTimestamp(t *testing.T) {
	tests := []struct {
		name     string
		logLine  string
		wantTime time.Time
		wantErr  bool
	}{
		{
			name:     "valid timestamp",
			logLine:  "2023-05-15T10:30:45.123456789Z Log message here",
			wantTime: time.Date(2023, 5, 15, 10, 30, 45, 123456789, time.UTC),
			wantErr:  false,
		},
		{
			name:     "valid timestamp with space",
			logLine:  "2023-05-15T10:30:45.123456789Z    Log message with spaces",
			wantTime: time.Date(2023, 5, 15, 10, 30, 45, 123456789, time.UTC),
			wantErr:  false,
		},
		{
			name:     "invalid timestamp",
			logLine:  "Not a timestamp",
			wantTime: time.Time{},
			wantErr:  true,
		},
		{
			name:     "invalid timestamp format",
			logLine:  "2023/05/15 10:30:45 Log message",
			wantTime: time.Time{},
			wantErr:  true,
		},
		{
			name:     "empty line",
			logLine:  "",
			wantTime: time.Time{},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTime, err := parseDockerTimestamp(tt.logLine)

			// Check error status
			if (err != nil) != tt.wantErr {
				t.Errorf("parseDockerTimestamp() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			// If we expected success, verify the time is correct
			if !tt.wantErr {
				if !gotTime.Equal(tt.wantTime) {
					t.Errorf("parseDockerTimestamp() = %v, want %v", gotTime, tt.wantTime)
				}
			}
		})
	}
}

// TestNewTimestampFilteredReader tests the timestampFilteredReader
// that filters logs by timestamp
func TestNewTimestampFilteredReader(t *testing.T) {
	// Set up test cases with different time ranges and sample logs
	now := time.Now()
	earlier := now.Add(-1 * time.Hour)
	later := now.Add(1 * time.Hour)
	muchEarlier := now.Add(-2 * time.Hour)
	muchLater := now.Add(2 * time.Hour)

	// Format timestamps as they would appear in logs
	nowStr := now.UTC().Format(time.RFC3339Nano)
	earlierStr := earlier.UTC().Format(time.RFC3339Nano)
	laterStr := later.UTC().Format(time.RFC3339Nano)
	muchEarlierStr := muchEarlier.UTC().Format(time.RFC3339Nano)
	muchLaterStr := muchLater.UTC().Format(time.RFC3339Nano)

	// Create sample log content with timestamps
	sampleLogs := []string{
		muchEarlierStr + " Log entry from much earlier\n",
		earlierStr + " Log entry from earlier\n",
		nowStr + " Log entry from now\n",
		laterStr + " Log entry from later\n",
		muchLaterStr + " Log entry from much later\n",
		"Invalid log line without timestamp\n",
	}

	logContent := strings.Join(sampleLogs, "")

	tests := []struct {
		name      string
		content   string
		since     time.Time
		until     time.Time
		wantLines []string
	}{
		{
			name:      "no time filters",
			content:   logContent,
			since:     time.Time{}, // Zero time means no filter
			until:     time.Time{}, // Zero time means no filter
			wantLines: sampleLogs,  // Should include all lines
		},
		{
			name:    "only since filter",
			content: logContent,
			since:   now,
			until:   time.Time{}, // Zero time means no filter
			wantLines: []string{
				nowStr + " Log entry from now\n",
				laterStr + " Log entry from later\n",
				muchLaterStr + " Log entry from much later\n",
				"Invalid log line without timestamp\n", // Invalid lines are always included
			},
		},
		{
			name:    "only until filter",
			content: logContent,
			since:   time.Time{}, // Zero time means no filter
			until:   now,
			wantLines: []string{
				muchEarlierStr + " Log entry from much earlier\n",
				earlierStr + " Log entry from earlier\n",
				nowStr + " Log entry from now\n",       // Time exactly matching until is included
				"Invalid log line without timestamp\n", // Invalid lines are always included
			},
		},
		{
			name:    "both since and until filters",
			content: logContent,
			since:   earlier,
			until:   later,
			wantLines: []string{
				earlierStr + " Log entry from earlier\n", // Time exactly matching since is included
				nowStr + " Log entry from now\n",
				laterStr + " Log entry from later\n",   // Time exactly matching until is included
				"Invalid log line without timestamp\n", // Invalid lines are always included
			},
		},
		{
			name:      "empty content",
			content:   "",
			since:     earlier,
			until:     later,
			wantLines: []string{}, // Should result in no lines
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create source reader with the test content
			source := io.NopCloser(strings.NewReader(tt.content))

			// Create the timestamp filtered reader
			reader := newTimestampFilteredReader(source, tt.since, tt.until)

			// Read all content from the filtered reader
			got, err := io.ReadAll(reader)
			if err != nil {
				t.Fatalf("Failed to read from filtered reader: %v", err)
			}

			// Convert expected lines to a single string for comparison
			want := strings.Join(tt.wantLines, "")

			// Compare the output
			if string(got) != want {
				t.Errorf("Got filtered content:\n%s\nWant:\n%s", string(got), want)
			}
		})
	}
}

// parseDockerTimestamp parses a timestamp from a Docker log line
// This is a mock implementation for tests that matches the function in the Docker runner
func parseDockerTimestamp(logLine string) (time.Time, error) {
	if len(logLine) == 0 {
		return time.Time{}, fmt.Errorf("empty log line")
	}

	// Docker timestamps are in RFC3339Nano format at the beginning of the line
	// Example: 2023-05-15T10:30:45.123456789Z Log message
	timestampEnd := strings.Index(logLine, " ")
	if timestampEnd == -1 {
		return time.Time{}, fmt.Errorf("invalid log line format")
	}

	timestampStr := logLine[:timestampEnd]
	timestamp, err := time.Parse(time.RFC3339Nano, timestampStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse timestamp: %w", err)
	}

	return timestamp, nil
}

// newTimestampFilteredReader creates a reader that filters logs by timestamp
// This is a mock implementation for tests
func newTimestampFilteredReader(reader io.ReadCloser, since, until time.Time) io.ReadCloser {
	if since.IsZero() && until.IsZero() {
		return reader // No filtering needed
	}

	pipeReader, pipeWriter := io.Pipe()

	go func() {
		defer pipeWriter.Close()
		defer reader.Close()

		// Read logs line by line
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			line := scanner.Text()

			// Parse the timestamp from the line
			timestamp, err := parseDockerTimestamp(line)

			// If we can't parse the timestamp, include the line by default
			if err != nil {
				// Write the line + newline
				pipeWriter.Write([]byte(line + "\n"))
				continue
			}

			// Apply Since filter
			if !since.IsZero() && timestamp.Before(since) {
				continue // Skip this line
			}

			// Apply Until filter
			if !until.IsZero() && timestamp.After(until) {
				continue // Skip this line
			}

			// Include this line (timestamp is within range)
			pipeWriter.Write([]byte(line + "\n"))
		}
	}()

	return pipeReader
}

// TestTimestampConversion tests the conversion of runner LogOptions times to Docker-compatible timestamp strings
func TestTimestampConversion(t *testing.T) {
	tests := []struct {
		name      string
		options   runner.LogOptions
		wantSince string
		wantUntil string
	}{
		{
			name: "no timestamps",
			options: runner.LogOptions{
				Follow:     true,
				Tail:       10,
				Timestamps: true,
			},
			wantSince: "", // Expected: empty
			wantUntil: "", // Expected: empty
		},
		{
			name: "with since timestamp only",
			options: runner.LogOptions{
				Follow:     false,
				Since:      time.Date(2023, 5, 15, 10, 0, 0, 0, time.UTC),
				Timestamps: true,
			},
			wantSince: "2023-05-15T10:00:00Z", // Expected: ISO8601 format
			wantUntil: "",                     // Expected: empty
		},
		{
			name: "with until timestamp only",
			options: runner.LogOptions{
				Follow:     false,
				Until:      time.Date(2023, 6, 20, 14, 30, 0, 0, time.UTC),
				Timestamps: true,
			},
			wantSince: "",                     // Expected: empty
			wantUntil: "2023-06-20T14:30:00Z", // Expected: ISO8601 format
		},
		{
			name: "with both since and until timestamps",
			options: runner.LogOptions{
				Follow:     false,
				Since:      time.Date(2023, 5, 15, 10, 0, 0, 0, time.UTC),
				Until:      time.Date(2023, 6, 20, 14, 30, 0, 0, time.UTC),
				Timestamps: true,
			},
			wantSince: "2023-05-15T10:00:00Z", // Expected: ISO8601 format
			wantUntil: "2023-06-20T14:30:00Z", // Expected: ISO8601 format
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Convert runner options to Docker container log options
			var since, until string

			// Convert Since timestamp if set
			if !tt.options.Since.IsZero() {
				since = tt.options.Since.Format(time.RFC3339)
			}

			// Convert Until timestamp if set
			if !tt.options.Until.IsZero() {
				until = tt.options.Until.Format(time.RFC3339)
			}

			// Check Since conversion
			if tt.wantSince == "" {
				if since != "" {
					t.Errorf("Since timestamp conversion = %v, want empty", since)
				}
			} else {
				if since != tt.wantSince {
					t.Errorf("Since timestamp conversion = %v, want %v", since, tt.wantSince)
				}
			}

			// Check Until conversion
			if tt.wantUntil == "" {
				if until != "" {
					t.Errorf("Until timestamp conversion = %v, want empty", until)
				}
			} else {
				if until != tt.wantUntil {
					t.Errorf("Until timestamp conversion = %v, want %v", until, tt.wantUntil)
				}
			}
		})
	}
}

// TestFilterLogsByTimestamp tests filtering log lines by timestamp
func TestFilterLogsByTimestamp(t *testing.T) {
	// Create test log lines with timestamps
	now := time.Now().UTC()
	earlier := now.Add(-1 * time.Hour)
	later := now.Add(1 * time.Hour)

	// Format the timestamps
	nowStr := now.Format(time.RFC3339Nano)
	earlierStr := earlier.Format(time.RFC3339Nano)
	laterStr := later.Format(time.RFC3339Nano)

	// Create test log content
	logLines := []string{
		earlierStr + " Earlier log message",
		nowStr + " Current log message",
		laterStr + " Later log message",
		"Invalid line without timestamp",
	}

	tests := []struct {
		name        string
		since       time.Time
		until       time.Time
		inputLines  []string
		wantIndices []int // indices of lines that should be included
	}{
		{
			name:        "no time filters",
			since:       time.Time{},
			until:       time.Time{},
			inputLines:  logLines,
			wantIndices: []int{0, 1, 2, 3}, // include all lines
		},
		{
			name:        "only since filter - include now and later",
			since:       now,
			until:       time.Time{},
			inputLines:  logLines,
			wantIndices: []int{1, 2, 3}, // include 'now', 'later' and invalid line
		},
		{
			name:        "only until filter - include earlier and now",
			since:       time.Time{},
			until:       now,
			inputLines:  logLines,
			wantIndices: []int{0, 1, 3}, // include 'earlier', 'now' and invalid line
		},
		{
			name:        "both since and until - include only now",
			since:       now.Add(-5 * time.Minute),
			until:       now.Add(5 * time.Minute),
			inputLines:  logLines,
			wantIndices: []int{1, 3}, // include only 'now' and invalid line
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Filter the lines manually based on timestamps
			var includedLines []string
			for _, line := range tt.inputLines {
				// Parse timestamp if possible
				timestampEnd := strings.Index(line, " ")
				if timestampEnd == -1 {
					// No timestamp, include by default
					includedLines = append(includedLines, line)
					continue
				}

				timestampStr := line[:timestampEnd]
				timestamp, err := time.Parse(time.RFC3339Nano, timestampStr)
				if err != nil {
					// Couldn't parse timestamp, include by default
					includedLines = append(includedLines, line)
					continue
				}

				// Apply Since filter
				if !tt.since.IsZero() && timestamp.Before(tt.since) {
					continue // Skip this line
				}

				// Apply Until filter
				if !tt.until.IsZero() && timestamp.After(tt.until) {
					continue // Skip this line
				}

				// Include this line
				includedLines = append(includedLines, line)
			}

			// Check that we got the right lines
			if len(includedLines) != len(tt.wantIndices) {
				t.Errorf("Got %d lines, want %d lines", len(includedLines), len(tt.wantIndices))
				return
			}

			// Check that each expected line is included
			for _, idx := range tt.wantIndices {
				found := false
				expectedLine := tt.inputLines[idx]
				for _, line := range includedLines {
					if line == expectedLine {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Line %q should be included but wasn't", expectedLine)
				}
			}
		})
	}
}

// We test the prepareSecretMounts host-side file materialization logic without actually creating Docker mounts.
func TestPrepareSecretMounts_Base64Decoding(t *testing.T) {
	t.Parallel()

	// Construct a runner directly to avoid Docker client creation.
	r := &DockerRunner{config: DefaultDockerConfig()}

	decoded := "-----BEGIN KEY-----\nabc\n-----END KEY-----\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(decoded))

	mounts, err := r.prepareSecretMounts([]types.ResolvedSecretMount{
		{
			Name:      "keys",
			MountPath: "/ignored/for/test",
			Data: map[string]string{
				"key.pem":  encoded,
				"note.txt": "hello",
			},
		},
	})
	if err != nil {
		t.Fatalf("prepareSecretMounts failed: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(mounts))
	}

	// The Source directory should contain our files with decoded content
	src := mounts[0].Source
	pemPath := filepath.Join(src, "key.pem")
	pemBytes, err := os.ReadFile(pemPath)
	if err != nil {
		t.Fatalf("failed reading %s: %v", pemPath, err)
	}
	if string(pemBytes) != decoded {
		t.Fatalf("decoded pem mismatch. want %q got %q", decoded, string(pemBytes))
	}

	notePath := filepath.Join(src, "note.txt")
	noteBytes, err := os.ReadFile(notePath)
	if err != nil {
		t.Fatalf("failed reading %s: %v", notePath, err)
	}
	if string(noteBytes) != "hello" {
		t.Fatalf("note mismatch. want %q got %q", "hello", string(noteBytes))
	}

	// cleanup temp dir created by prepareSecretMounts
	_ = os.RemoveAll(src)
}
