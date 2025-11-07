package cmd

import (
	"testing"
	"time"

	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
)

// Mock time.Now for testing
var timeNow = time.Now

func TestEffectiveCmdNS(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		defaultNS string
		expected  string
	}{
		{"empty namespace", "", "", ""},
		{"explicit namespace provided", "test-ns", "default-ns", "test-ns"},
		{"use default namespace when explicit not provided", "", "default-ns", "default-ns"},
		{"empty when no namespace specified", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save original value
			original := viper.GetString("contexts.default.defaultNamespace")
			// Clean up after test
			defer viper.Set("contexts.default.defaultNamespace", original)

			viper.Set("contexts.default.defaultNamespace", tt.defaultNS)
			result := effectiveCmdNS(tt.namespace)
			assert.Equal(t, tt.expected, result, "effectiveCmdNS() = %v, want %v", result, tt.expected)
		})
	}
}

func TestParseTargetExpression(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected types.ResourceType
	}{
		{"instance full name", "instance/my-instance", types.ResourceTypeInstance},
		{"instance short name", "inst/my-instance", types.ResourceTypeInstance},
		{"service full name", "service/my-service", types.ResourceTypeService},
		{"service short name", "svc/my-service", types.ResourceTypeService},
		{"no prefix", "my-resource", ""},
		{"invalid prefix", "invalid/resource", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseTargetExpression(tt.input)
			assert.Equal(t, tt.expected, result, "parseTargetExpression() = %v, want %v", result, tt.expected)
		})
	}
}

func TestFormatAge(t *testing.T) {
	tests := []struct {
		name     string
		duration float64 // in seconds
		expected string
	}{
		{"just now", 30, "Just now"},
		{"minutes", 120, "2m"},
		{"hours", 3600, "1h"},
		{"days", 86400, "1d"},
		{"months", 2592000, "1mo"},
		{"years", 31536000, "1y"},
	}

	now := timeNow()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			timestamp := now.Add(-(time.Duration(tc.duration) * time.Second))
			age := formatAge(timestamp)
			assert.Equal(t, tc.expected, age)
		})
	}
}
