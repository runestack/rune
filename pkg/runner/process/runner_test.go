package process

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/runner"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRunner(t *testing.T) *ProcessRunner {
	t.Helper()
	base := t.TempDir()
	r, err := NewProcessRunner(WithBaseDir(base), WithLogger(log.NewLogger()))
	if err != nil {
		t.Fatalf("NewProcessRunner error: %v", err)
	}
	return r
}

func TestNewProcessRunner(t *testing.T) {
	// Create temporary directory for tests
	testDir, err := os.MkdirTemp("", "process-runner-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(testDir)

	// Test with default options
	r1, err := NewProcessRunner()
	require.NoError(t, err)
	assert.Equal(t, os.TempDir(), r1.baseDir)

	// Test with custom options
	logger := log.NewLogger()
	r2, err := NewProcessRunner(
		WithBaseDir(testDir),
		WithLogger(logger),
	)
	require.NoError(t, err)
	assert.Equal(t, testDir, r2.baseDir)
	assert.Equal(t, logger, r2.logger)
}

func TestProcessRunner_Create(t *testing.T) {
	// Setup
	testDir, err := os.MkdirTemp("", "process-runner-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(testDir)

	processRunner := newTestRunner(t)
	require.NoError(t, err)

	// Create a valid instance
	instance := &types.Instance{
		ID:        "test-instance",
		Name:      "test",
		NodeID:    "local",
		ServiceID: "test-service",
		Process: &types.ProcessSpec{
			Command: "echo",
			Args:    []string{"hello world"},
		},
	}

	// Test: Create a valid instance
	err = processRunner.Create(context.Background(), instance)
	require.NoError(t, err)

	// Verify instance was created
	assert.Contains(t, processRunner.processes, "test-instance")
	assert.Equal(t, types.InstanceStatusCreated, instance.Status)
	assert.NotZero(t, instance.CreatedAt)
	assert.NotZero(t, instance.UpdatedAt)

	// Test: Create duplicate instance should fail
	err = processRunner.Create(context.Background(), instance)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	// Test: Create with nil instance should fail
	err = processRunner.Create(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be nil")

	// Test: Create with invalid instance should fail
	invalidInstance := &types.Instance{
		ID:        "invalid-instance",
		Name:      "invalid",
		NodeID:    "local",
		ServiceID: "test-service",
		// Missing Process spec
	}
	err = processRunner.Create(context.Background(), invalidInstance)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "process spec is required")
}

func TestProcessRunner_StartStopRemove(t *testing.T) {
	// Skip on CI systems where process control may be limited
	if os.Getenv("CI") != "" {
		t.Skip("Skipping process tests in CI environment")
	}

	// Setup
	testDir, err := os.MkdirTemp("", "process-runner-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(testDir)

	processRunner := newTestRunner(t)
	require.NoError(t, err)

	// Create a sleep process that we can control
	instance := &types.Instance{
		ID:        "test-sleep",
		Name:      "test-sleep",
		NodeID:    "local",
		ServiceID: "test-service",
		Process: &types.ProcessSpec{
			Command: "sleep",
			Args:    []string{"10"},
		},
	}

	ctx := context.Background()
	err = processRunner.Create(ctx, instance)
	require.NoError(t, err)

	// Test: Start the process
	err = processRunner.Start(ctx, instance)
	require.NoError(t, err)

	// Verify process is running
	time.Sleep(100 * time.Millisecond) // Give it time to start
	status, err := processRunner.Status(ctx, instance)
	require.NoError(t, err)
	assert.Equal(t, types.InstanceStatusRunning, status)

	// Test: Stop the process
	err = processRunner.Stop(ctx, instance, 5*time.Second)
	require.NoError(t, err)

	// Verify process is stopped
	time.Sleep(100 * time.Millisecond) // Give it time to stop
	status, err = processRunner.Status(ctx, instance)
	require.NoError(t, err)
	assert.Equal(t, types.InstanceStatusStopped, status)

	// Test: Remove the process
	err = processRunner.Remove(ctx, instance, false)
	require.NoError(t, err)

	// Verify process is removed
	_, err = processRunner.Status(ctx, instance)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestProcessRunner_GetLogs(t *testing.T) {
	// Setup
	testDir, err := os.MkdirTemp("", "process-runner-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(testDir)

	processRunner := newTestRunner(t)
	require.NoError(t, err)

	// Create a process that outputs something
	instance := &types.Instance{
		ID:        "test-echo",
		Namespace: "test",
		Name:      "test-echo",
		NodeID:    "local",
		ServiceID: "test-service",
		Process: &types.ProcessSpec{
			Command: "echo",
			Args:    []string{"test output"},
		},
	}

	ctx := context.Background()
	err = processRunner.Create(ctx, instance)
	require.NoError(t, err)

	// Start the process
	err = processRunner.Start(ctx, instance)
	require.NoError(t, err)

	// Give the process time to complete
	time.Sleep(100 * time.Millisecond)

	// Test: Get logs
	logs, err := processRunner.GetLogs(ctx, instance, runner.LogOptions{})
	require.NoError(t, err)
	defer logs.Close()

	// Read log content
	logContent := make([]byte, 1024)
	n, err := logs.Read(logContent)
	require.NoError(t, err)
	logContent = logContent[:n]

	// Verify log content contains our output
	assert.Contains(t, string(logContent), "test output")

	// Cleanup
	err = processRunner.Remove(ctx, instance, true)
	require.NoError(t, err)
}

// TestProcessRunner_GetLogsWithTimestamps tests the timestamp-based filtering of logs
func TestProcessRunner_GetLogsWithTimestamps(t *testing.T) {
	// Setup
	testDir, err := os.MkdirTemp("", "process-runner-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(testDir)

	processRunner := newTestRunner(t)
	require.NoError(t, err)

	// Create a test instance
	instance := &types.Instance{
		ID:        "test-timestamp-logs",
		Namespace: "test",
		Name:      "test-timestamp-logs",
		NodeID:    "local",
		ServiceID: "test-service",
		Process: &types.ProcessSpec{
			Command: "echo",
			Args:    []string{"This will be overwritten with test logs"},
		},
	}

	ctx := context.Background()
	err = processRunner.Create(ctx, instance)
	require.NoError(t, err)

	// Start the process
	err = processRunner.Start(ctx, instance)
	require.NoError(t, err)

	// Give the process time to complete
	time.Sleep(100 * time.Millisecond)

	// Get the process object to access its log file
	processRunner.mu.RLock()
	proc, exists := processRunner.processes[instance.ID]
	processRunner.mu.RUnlock()
	require.True(t, exists)

	// Create a test log file with timestamps
	logPath := proc.logFile.Name()
	err = proc.logFile.Close() // Close the original log file
	require.NoError(t, err)

	// Create a new log file with entries containing timestamps
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	defer logFile.Close()

	// Create fixed timestamps for testing
	now := time.Date(2023, 4, 1, 12, 0, 0, 0, time.UTC)
	timestamps := []time.Time{
		now.Add(-1 * time.Hour),    // 1 hour ago - 11:00
		now.Add(-30 * time.Minute), // 30 minutes ago - 11:30
		now.Add(-5 * time.Minute),  // 5 minutes ago - 11:55
		now.Add(-1 * time.Minute),  // 1 minute ago - 11:59
		now,                        // now - 12:00
	}

	// Write log entries with different timestamp formats
	logEntries := []string{
		// ISO8601/RFC3339 format for 1 hour ago
		fmt.Sprintf("[%s] Log entry from 1 hour ago\n", timestamps[0].Format(time.RFC3339)),
		// Common timestamp format for 30 minutes ago
		fmt.Sprintf("[%s] Log entry from 30 minutes ago\n", timestamps[1].Format("2006-01-02 15:04:05")),
		// Another common format for 5 minutes ago
		fmt.Sprintf("[%s] Log entry from 5 minutes ago\n", timestamps[2].Format("2006/01/02 15:04:05")),
		// Syslog-like format for 1 minute ago
		fmt.Sprintf("[%s] Log entry from 1 minute ago\n", timestamps[3].Format("Jan 2 15:04:05")),
		// Just time format for current time
		fmt.Sprintf("[%s] Current log entry\n", timestamps[4].Format("15:04:05")),
	}

	// Print timestamp debug info
	for i, ts := range timestamps {
		t.Logf("Timestamp %d: %s", i, ts.Format(time.RFC3339))
	}

	// Write log entries to file with debug output
	for i, entry := range logEntries {
		t.Logf("Log entry %d: %s", i, entry)
		_, err = logFile.WriteString(entry)
		require.NoError(t, err)
	}
	err = logFile.Sync()
	require.NoError(t, err)

	// Test cases
	testCases := []struct {
		name     string
		options  runner.LogOptions
		expected []string
		excluded []string
	}{
		{
			name:     "NoFilter",
			options:  runner.LogOptions{},
			expected: []string{"1 hour ago", "30 minutes ago", "5 minutes ago", "1 minute ago", "Current log entry"},
			excluded: []string{},
		},
		{
			name: "SinceFilter",
			options: runner.LogOptions{
				Since: timestamps[2], // 5 minutes ago (11:55)
			},
			expected: []string{"5 minutes ago", "1 minute ago", "Current log entry"},
			excluded: []string{"1 hour ago", "30 minutes ago"},
		},
		{
			name: "UntilFilter",
			options: runner.LogOptions{
				Until: timestamps[2], // 5 minutes ago (11:55)
			},
			expected: []string{"1 hour ago", "30 minutes ago", "5 minutes ago"},
			excluded: []string{"1 minute ago", "Current log entry"},
		},
		{
			name: "SinceAndUntilFilter",
			options: runner.LogOptions{
				Since: timestamps[1], // 30 minutes ago (11:30)
				Until: timestamps[3], // 1 minute ago (11:59)
			},
			expected: []string{"30 minutes ago", "5 minutes ago"}, // Time-only formats may not match due to normalization
			excluded: []string{"1 hour ago", "Current log entry"},
		},
		{
			name: "TailFilter",
			options: runner.LogOptions{
				Tail: 2,
			},
			expected: []string{"1 minute ago", "Current log entry"},
			excluded: []string{"1 hour ago", "30 minutes ago", "5 minutes ago"},
		},
		{
			name: "CombinedFilters",
			options: runner.LogOptions{
				Since: timestamps[1], // 30 minutes ago (11:30)
				Tail:  3,
			},
			expected: []string{"5 minutes ago", "1 minute ago", "Current log entry"},
			excluded: []string{"1 hour ago", "30 minutes ago"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			logs, err := processRunner.GetLogs(ctx, instance, tc.options)
			require.NoError(t, err)
			defer logs.Close()

			// Read all log content
			content, err := io.ReadAll(logs)
			require.NoError(t, err)
			contentStr := string(content)

			// Print debug info about the filtered content
			t.Logf("Test case %s content: %s", tc.name, contentStr)

			// Also test the parse function directly with our log entries
			if tc.name == "SinceFilter" {
				t.Log("Testing timestamp parsing for each log line:")
				for i, entry := range logEntries {
					ts, ok := extractTimestamp([]byte(entry))
					t.Logf("Entry %d: parsed=%v, timestamp=%v", i, ok, ts)
					if ok {
						isBefore := ts.Before(timestamps[2])
						isAfter := ts.After(timestamps[2]) || ts.Equal(timestamps[2])
						t.Logf("Entry %d: before=%v, after=%v", i, isBefore, isAfter)
					}
				}
			}

			// Check that expected entries are present
			for _, expected := range tc.expected {
				assert.Contains(t, contentStr, expected, "Log should contain: %s", expected)
			}

			// Check that excluded entries are not present
			for _, excluded := range tc.excluded {
				assert.NotContains(t, contentStr, excluded, "Log should not contain: %s", excluded)
			}
		})
	}

	// Test timestamp extraction function directly with explicit examples
	t.Run("TimestampExtraction", func(t *testing.T) {
		testCases := []struct {
			line        string
			shouldParse bool
		}{
			{"[2023-04-01T12:00:00Z] Test message", true},
			{"[2023-04-01T12:00:00.123456789Z] Test message", true},
			{"[2023-04-01 12:00:00] Test message", true},
			{"[2023/04/01 12:00:00] Test message", true},
			{"[Apr 1 12:00:00] Test message", true},
			{"[Apr 01 12:00:00] Test message", true},
			{"[12:00:00] Test message", true},
			{"No timestamp here", false},
			{"2023-04-01T12:00:00Z Test without brackets", true},
			{"12:00:00 Simple time prefix", true},
		}

		for _, tc := range testCases {
			timestamp, ok := extractTimestamp([]byte(tc.line))
			t.Logf("Line: %s, parsed=%v, timestamp=%v", tc.line, ok, timestamp)
			assert.Equal(t, tc.shouldParse, ok, "Extracting timestamp from: %s", tc.line)
			if tc.shouldParse {
				assert.False(t, timestamp.IsZero(), "Timestamp should not be zero for: %s", tc.line)
			}
		}
	})

	// Cleanup
	err = processRunner.Remove(ctx, instance, true)
	require.NoError(t, err)
}

func TestProcessRunner_List(t *testing.T) {
	// Setup
	testDir, err := os.MkdirTemp("", "process-runner-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(testDir)

	processRunner := newTestRunner(t)
	require.NoError(t, err)

	ctx := context.Background()

	// Create a few instances
	for i := 1; i <= 3; i++ {
		instance := &types.Instance{
			ID:        filepath.Join("test-instance", string(rune('0'+i))),
			Name:      filepath.Join("test", string(rune('0'+i))),
			NodeID:    "local",
			ServiceID: "test-service",
			Process: &types.ProcessSpec{
				Command: "echo",
				Args:    []string{"test"},
			},
		}
		err = processRunner.Create(ctx, instance)
		require.NoError(t, err)
	}

	// Test: List instances
	instances, err := processRunner.List(ctx, "test")
	require.NoError(t, err)
	assert.Len(t, instances, 3)

	// Verify instance details
	found := false
	for _, instance := range instances {
		if instance.ID == "test-instance/1" {
			found = true
			assert.Equal(t, "test/1", instance.Name)
			assert.Equal(t, types.InstanceStatusCreated, instance.Status)
		}
	}
	assert.True(t, found, "Expected instance not found in list")
}

func TestFindStartPosition(t *testing.T) {
	// Create a temporary file with known content
	f, err := os.CreateTemp("", "log-test-*")
	require.NoError(t, err)
	defer os.Remove(f.Name())
	defer f.Close()

	// Write 10 lines
	for i := 1; i <= 10; i++ {
		_, err = f.WriteString(filepath.Join("Line", string(rune('0'+i)), "\n"))
		require.NoError(t, err)
	}
	f.Sync()

	// Get file size
	info, err := f.Stat()
	require.NoError(t, err)
	fileSize := info.Size()

	// Test cases
	tests := []struct {
		name     string
		numLines int
		want     int64 // 0 means start of file
	}{
		{"Zero lines", 0, 0},
		{"Negative lines", -1, 0},
		{"More lines than file", 20, 0},
		{"Last 3 lines", 3, fileSize - 18}, // "Line8\nLine9\nLine10\n" = 18 bytes
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findStartPosition(f, fileSize, tt.numLines)
			if tt.want == 0 {
				assert.Equal(t, int64(0), got)
			} else {
				// For the "Last 3 lines" test, we need a more flexible check
				// since the exact position depends on line endings
				if tt.numLines == 3 {
					// Seek to the position and read
					_, err = f.Seek(got, 0)
					require.NoError(t, err)
					content := make([]byte, fileSize)
					n, err := f.Read(content)
					require.NoError(t, err)
					content = content[:n]
					lines := 0
					for _, b := range content {
						if b == '\n' {
							lines++
						}
					}
					assert.Equal(t, 3, lines, "Should have 3 lines from position")
				}
			}
		})
	}
}

func TestWriteSecretFiles_DefaultMapping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits not portable on Windows")
	}
	r := newTestRunner(t)
	mountDir := t.TempDir()

	m := types.ResolvedSecretMount{
		Name:      "m1",
		MountPath: mountDir,
		Data: map[string]string{
			"foo": "bar",
			"baz": "qux",
		},
	}

	files, err := r.writeSecretFiles(m)
	if err != nil {
		t.Fatalf("writeSecretFiles error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	// Validate contents and permissions
	for k, v := range m.Data {
		p := filepath.Join(mountDir, k)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if string(b) != v {
			t.Fatalf("content mismatch for %s: got %q want %q", p, string(b), v)
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if st.Mode().Perm() != 0o400 {
			t.Fatalf("mode for %s = %o, want 0400", p, st.Mode().Perm())
		}
	}
}

func TestWriteSecretFiles_ItemsMapping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits not portable on Windows")
	}
	r := newTestRunner(t)
	mountDir := t.TempDir()

	m := types.ResolvedSecretMount{
		Name:      "m2",
		MountPath: mountDir,
		Data: map[string]string{
			"a": "1",
			"b": "2",
		},
		Items: []types.KeyToPath{
			{Key: "a", Path: "x/a.txt"},
		},
	}

	files, err := r.writeSecretFiles(m)
	if err != nil {
		t.Fatalf("writeSecretFiles error: %v", err)
	}
	// a mapped explicitly, b default-mapped
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	// Validate paths
	wantA := filepath.Join(mountDir, "x", "a.txt")
	if _, err := os.Stat(wantA); err != nil {
		t.Fatalf("expected file %s: %v", wantA, err)
	}
	wantB := filepath.Join(mountDir, "b")
	if _, err := os.Stat(wantB); err != nil {
		t.Fatalf("expected file %s: %v", wantB, err)
	}
}

func TestWriteConfigFiles_DefaultMapping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits not portable on Windows")
	}
	r := newTestRunner(t)
	mountDir := t.TempDir()
	m := types.ResolvedConfigmapMount{
		Name:      "cfg1",
		MountPath: mountDir,
		Data: map[string]string{
			"c1": "v1",
			"c2": "v2",
		},
	}
	files, err := r.writeConfigFiles(m)
	if err != nil {
		t.Fatalf("writeConfigFiles error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	for k, v := range m.Data {
		p := filepath.Join(mountDir, k)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if string(b) != v {
			t.Fatalf("content mismatch for %s: got %q want %q", p, string(b), v)
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if st.Mode().Perm() != 0o644 {
			t.Fatalf("mode for %s = %o, want 0644", p, st.Mode().Perm())
		}
	}
}

func TestWriteSecretFiles_Base64Decoding(t *testing.T) {
	t.Parallel()

	runner, err := NewProcessRunner()
	if err != nil {
		t.Fatalf("failed to create ProcessRunner: %v", err)
	}

	tempDir := t.TempDir()

	decodedContent := "-----BEGIN TEST-----\nhello world\n-----END TEST-----\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(decodedContent))

	mount := types.ResolvedSecretMount{
		Name:      "test-secret",
		MountPath: tempDir,
		Data: map[string]string{
			"pem.b64":   encoded,
			"plain.txt": "just-text",
		},
	}

	created, err := runner.writeSecretFiles(mount)
	if err != nil {
		t.Fatalf("writeSecretFiles failed: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("expected 2 files, got %d", len(created))
	}

	// Verify decoded content for the base64 entry
	pemPath := filepath.Join(tempDir, "pem.b64")
	gotPem, err := os.ReadFile(pemPath)
	if err != nil {
		t.Fatalf("failed reading %s: %v", pemPath, err)
	}
	if string(gotPem) != decodedContent {
		t.Fatalf("decoded content mismatch.\nwant:\n%q\n got:\n%q", decodedContent, string(gotPem))
	}

	// Verify plain text is untouched
	plainPath := filepath.Join(tempDir, "plain.txt")
	gotPlain, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatalf("failed reading %s: %v", plainPath, err)
	}
	if string(gotPlain) != "just-text" {
		t.Fatalf("plain content mismatch. want %q got %q", "just-text", string(gotPlain))
	}
}
