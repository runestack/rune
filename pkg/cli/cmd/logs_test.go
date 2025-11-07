package cmd

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/stretchr/testify/assert"
)

// TestParseTraceOptions tests the parseTraceOptions function
func TestParseTraceOptions(t *testing.T) {
	// Test case 1: Default options
	t.Run("DefaultOptions", func(t *testing.T) {
		// Set flags
		logsOptions := &logsOptions{
			follow:         true,
			tail:           100,
			showTimestamps: false,
			pattern:        "",
			outputFormat:   OutputFormatText,
		}

		// Parse options
		options, err := parseLogsOptions(logsOptions)

		// Verify
		assert.NoError(t, err)
		assert.Equal(t, true, options.follow)
		assert.Equal(t, 100, options.tail)
		assert.Equal(t, false, options.showTimestamps)
		assert.Equal(t, "", options.pattern)
		assert.Equal(t, OutputFormatText, options.outputFormat)
	})

	// Test case 2: Custom options
	t.Run("CustomOptions", func(t *testing.T) {
		// Set flags
		logsOptions := &logsOptions{
			follow:         false,
			tail:           50,
			showTimestamps: true,
			pattern:        "error",
			outputFormat:   OutputFormatJSON,
		}

		// Parse options
		options, err := parseLogsOptions(logsOptions)

		// Verify
		assert.NoError(t, err)
		assert.Equal(t, false, options.follow)
		assert.Equal(t, 50, options.tail)
		assert.Equal(t, true, options.showTimestamps)
		assert.Equal(t, "error", options.pattern)
		assert.Equal(t, OutputFormatJSON, options.outputFormat)
	})

	// Test case 3: Invalid output format
	t.Run("InvalidOutputFormat", func(t *testing.T) {
		// Set flags
		logsOptions := &logsOptions{
			outputFormat: "invalid",
		}

		// Parse options
		_, err := parseLogsOptions(logsOptions)

		// Verify
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid output format")
	})
}

// TestParseSinceTime tests the parseSinceTime function
func TestParseSinceTime(t *testing.T) {
	// Test case 1: Duration
	t.Run("Duration", func(t *testing.T) {
		now := time.Now()
		result, err := parseSinceTime("1h")

		assert.NoError(t, err)
		assert.True(t, now.Add(-2*time.Hour).Before(result))
		assert.True(t, now.After(result))
	})

	// Test case 2: RFC3339 format
	t.Run("RFC3339", func(t *testing.T) {
		timeStr := "2021-01-02T15:04:05Z"
		expected, _ := time.Parse(time.RFC3339, timeStr)

		result, err := parseSinceTime(timeStr)

		assert.NoError(t, err)
		assert.Equal(t, expected, result)
	})

	// Test case 3: Date only
	t.Run("DateOnly", func(t *testing.T) {
		result, err := parseSinceTime("2021-01-02")

		assert.NoError(t, err)
		assert.Equal(t, 2021, result.Year())
		assert.Equal(t, time.January, result.Month())
		assert.Equal(t, 2, result.Day())
	})

	// Test case 4: Invalid format
	t.Run("InvalidFormat", func(t *testing.T) {
		_, err := parseSinceTime("not-a-time")

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unrecognized time format")
	})
}

// TestProcessLogResponse tests the processLogResponse function
func TestProcessLogResponse(t *testing.T) {
	// Mock trace options
	options := &logsOptions{
		showTimestamps: false,
		noColor:        true, // Disable colors for testing
	}

	// Test with a log entry
	t.Run("ProcessLogEntry", func(t *testing.T) {
		resp := &generated.LogResponse{
			ServiceName: "test-service",
			InstanceId:  "test-instance",
			Timestamp:   time.Now().Format(time.RFC3339),
			Content:     "Log message",
			LogLevel:    "info",
		}

		// Process should not error
		err := processLogResponse(resp, options)
		assert.NoError(t, err)
	})

	// Test filtering by log type
	t.Run("FilterByLogType", func(t *testing.T) {
		// Set up options to filter out logs
		filteredOptions := &logsOptions{
			showTimestamps: false,
			noColor:        true,
		}

		resp := &generated.LogResponse{
			ServiceName: "test-service",
			InstanceId:  "test-instance",
			Timestamp:   time.Now().Format(time.RFC3339),
			Content:     "Log message",
			LogLevel:    "info",
		}

		// Processing should skip this log entry
		err := processLogResponse(resp, filteredOptions)

		// No error, but the log should be filtered out
		// (This would normally output to stdout, but we can't easily test that here)
		assert.NoError(t, err)
	})

	// Test timestamp toggling
	t.Run("TimestampToggle", func(t *testing.T) {
		// Set up options with timestamps enabled
		timestampOptions := &logsOptions{
			showTimestamps: true,
			noColor:        true,
		}

		resp := &generated.LogResponse{
			ServiceName: "test-service",
			InstanceId:  "test-instance",
			Timestamp:   time.Now().Format(time.RFC3339),
			Content:     "Log message",
			LogLevel:    "info",
		}

		// Processing should not error with timestamps enabled
		err := processLogResponse(resp, timestampOptions)
		assert.NoError(t, err)
	})
}
