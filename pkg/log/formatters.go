package log

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// JSONFormatter formats log entries as JSON.
type JSONFormatter struct {
	TimestampFormat string // Format for timestamps
	EnableCaller    bool   // Enable caller information (default: false)
}

// Format formats the entry as JSON.
func (f *JSONFormatter) Format(entry *Entry) ([]byte, error) {
	data := make(map[string]interface{})

	// Add timestamp
	timestampFormat := time.RFC3339
	if f.TimestampFormat != "" {
		timestampFormat = f.TimestampFormat
	}
	data["timestamp"] = entry.Timestamp.Format(timestampFormat)

	// Add level
	data["level"] = entry.Level.String()

	// Add message
	data["message"] = entry.Message

	// Add caller if enabled
	if f.EnableCaller && entry.Caller != "" {
		data["caller"] = entry.Caller
	}

	// A field named like a standard key is kept under "fields.<key>" rather
	// than dropped, so a caller's choice of key cannot lose the value.
	for k, v := range entry.Fields {
		switch k {
		case "timestamp", "level", "message", "caller":
			data["fields."+k] = v
		default:
			data[k] = v
		}
	}

	// Marshal to JSON
	return json.Marshal(data)
}

// TextFormatter formats log entries as human-readable text.
type TextFormatter struct {
	TimestampFormat  string // Format for timestamps
	EnableCaller     bool   // Enable caller information (default: false)
	DisableColors    bool   // Disable color output
	ShortTimestamp   bool   // Use short timestamp without date (default: false)
	DisableTimestamp bool   // Disable timestamp output
}

// NewTextFormatter creates a new TextFormatter with sensible defaults.
func NewTextFormatter() *TextFormatter {
	return &TextFormatter{
		TimestampFormat: "15:04:05.000",
		EnableCaller:    false, // Don't show caller by default
		DisableColors:   false, // Enable colors by default
		ShortTimestamp:  false, // Use full timestamp by default (with date)
	}
}

// Format formats the entry as text.
func (f *TextFormatter) Format(entry *Entry) ([]byte, error) {
	// Determine timestamp format
	timestampFormat := "2006-01-02T15:04:05.000" // Default to full timestamp
	if f.ShortTimestamp {
		timestampFormat = "15:04:05.000" // Short timestamp if explicitly requested
	}
	if f.TimestampFormat != "" {
		timestampFormat = f.TimestampFormat // Override with custom format if specified
	}

	// Format timestamp
	timestamp := entry.Timestamp.Format(timestampFormat)

	// Format level with color
	level := entry.Level.String()
	if !f.DisableColors {
		level = colorizeLevel(entry.Level)
	}

	// Format caller (only if explicitly enabled)
	caller := ""
	if f.EnableCaller && entry.Caller != "" {
		if !f.DisableColors {
			caller = fmt.Sprintf(" (%s%s%s)", colorDim, entry.Caller, colorReset)
		} else {
			caller = fmt.Sprintf(" (%s)", entry.Caller)
		}
	}

	// Extract fields
	var fieldParts []string
	for k, v := range entry.Fields {
		if !f.DisableColors {
			// Use light blue color for field keys
			fieldParts = append(fieldParts, fmt.Sprintf("%s%s%s=%v", colorCyan, k, colorReset, v))
		} else {
			fieldParts = append(fieldParts, fmt.Sprintf("%s=%v", k, v))
		}
	}

	fields := ""
	if len(fieldParts) > 0 {
		fields = " " + strings.Join(fieldParts, " ")
	}

	// Format message with color (just use the default color)
	message := entry.Message

	// Add color to timestamp if enabled
	if !f.DisableColors {
		timestamp = colorDim + timestamp + colorReset
	}

	// Put together final log line
	logLine := fmt.Sprintf("%s %s%s %s%s\n",
		timestamp,
		level,
		caller,
		message,
		fields)

	return []byte(logLine), nil
}

// Color codes for terminal output
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorCyan   = "\033[36m" // Light blue/cyan for field keys
	colorDim    = "\033[90m" // Dim/gray color for timestamps, caller
)

// colorizeLevel adds color to log levels
func colorizeLevel(level Level) string {
	switch level {
	case DebugLevel:
		return colorBlue + "DBG" + colorReset
	case InfoLevel:
		return colorGreen + "INF" + colorReset
	case WarnLevel:
		return colorYellow + "WRN" + colorReset
	case ErrorLevel:
		return colorRed + "ERR" + colorReset
	case FatalLevel:
		return colorRed + "FTL" + colorReset
	default:
		return level.String() + colorReset
	}
}
