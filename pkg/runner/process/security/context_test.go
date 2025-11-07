package security

import (
	"os/exec"
	"runtime"
	"testing"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
)

func TestApplySecurityContext(t *testing.T) {
	// Create a test logger
	logger := log.NewLogger()

	// Test cases
	testCases := []struct {
		name        string
		sc          *types.ProcessSecurityContext
		expectError bool
		skipOnOS    string // Skip on specific OS
	}{
		{
			name:        "Nil security context",
			sc:          nil,
			expectError: false,
		},
		{
			name: "Empty security context",
			sc:   &types.ProcessSecurityContext{
				// Empty context
			},
			expectError: false,
		},
		{
			name: "Read-only filesystem",
			sc: &types.ProcessSecurityContext{
				ReadOnlyFS: true,
			},
			expectError: false, // Should only log a warning, not fail
		},
		{
			name: "With capabilities",
			sc: &types.ProcessSecurityContext{
				Capabilities: []string{"NET_BIND_SERVICE"},
			},
			expectError: false, // Just logs warning in current implementation
		},
		{
			name: "With syscall restrictions",
			sc: &types.ProcessSecurityContext{
				AllowedSyscalls: []string{"read", "write"},
				DeniedSyscalls:  []string{"mount"},
			},
			expectError: false, // Just logs warning in current implementation
		},
		{
			name: "With invalid user",
			sc: &types.ProcessSecurityContext{
				User: "this-user-should-not-exist-12345",
			},
			expectError: true,
			skipOnOS:    "windows", // Skip on Windows
		},
	}

	// Run test cases
	for _, tc := range testCases {
		// Skip tests as needed
		if tc.skipOnOS != "" && runtime.GOOS == tc.skipOnOS {
			t.Logf("Skipping test '%s' on %s", tc.name, runtime.GOOS)
			continue
		}

		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("echo", "test")
			err := ApplySecurityContext(cmd, tc.sc, logger)

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestApplyUserAndGroup(t *testing.T) {
	// Skip on Windows as it doesn't support this functionality
	if runtime.GOOS == "windows" {
		t.Skip("User/group switching not supported on Windows")
	}

	cmd := exec.Command("echo", "test")

	// Test with empty username
	err := applyUserAndGroup(cmd, "", "")
	assert.NoError(t, err)

	// Test with non-existent user
	err = applyUserAndGroup(cmd, "this-user-should-not-exist-12345", "")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to look up user")

	// Test with non-existent group
	// Using "nobody" user which typically exists on most systems
	err = applyUserAndGroup(cmd, "nobody", "this-group-should-not-exist-12345")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to look up group")
}
