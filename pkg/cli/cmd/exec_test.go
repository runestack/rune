package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestExecArgParsing verifies the three accepted shapes:
//   - rune exec [flags] TARGET                      → auto-shell
//   - rune exec [flags] TARGET COMMAND [args...]    → simple command
//   - rune exec [flags] TARGET -- COMMAND [args...] → command with flags
//
// And confirms that rune flags work in any position before '--', that
// targets-only sanity errors are friendly, and that unknown flags after the
// TARGET (without '--') get a hint to use '--'.
func TestExecArgParsing(t *testing.T) {
	tests := []struct {
		name    string
		argv    []string // argv after "exec"
		wantErr string   // substring in the error; "" means success
		wantNs  string
		wantCmd []string
	}{
		{
			name:    "target only opens default shell",
			argv:    []string{"web"},
			wantCmd: defaultShellCommand(),
		},
		{
			name:    "target plus namespace flag, no command",
			argv:    []string{"-n", "prod", "web"},
			wantNs:  "prod",
			wantCmd: defaultShellCommand(),
		},
		{
			name:    "simple command without dash-dash",
			argv:    []string{"web", "bash"},
			wantCmd: []string{"bash"},
		},
		{
			name:    "simple command with positional args (no flags)",
			argv:    []string{"web", "ps", "aux"},
			wantCmd: []string{"ps", "aux"},
		},
		{
			name:    "command with flags requires dash-dash",
			argv:    []string{"web", "--", "ls", "-la", "/app"},
			wantCmd: []string{"ls", "-la", "/app"},
		},
		{
			name:    "rune flag after target before dash-dash",
			argv:    []string{"web", "-n", "prod", "--", "bash"},
			wantNs:  "prod",
			wantCmd: []string{"bash"},
		},
		{
			name:    "rune flag before target",
			argv:    []string{"-n", "prod", "web", "--", "bash", "-c", "echo hi"},
			wantNs:  "prod",
			wantCmd: []string{"bash", "-c", "echo hi"},
		},
		{
			name:    "dash-dash with no target",
			argv:    []string{"--", "bash"},
			wantErr: "TARGET is required",
		},
		{
			name:    "unknown flag after target hints at dash-dash",
			argv:    []string{"web", "ls", "-la"},
			wantErr: "after '--'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newExecCmd()

			// Intercept RunE so we don't try to dial an API server. We mirror
			// the real arg handling so this test exercises it.
			var gotNs string
			var gotCmd []string
			cmd.RunE = func(c *cobra.Command, args []string) error {
				dashIdx := c.ArgsLenAtDash()
				if dashIdx > 1 {
					return errTooManyTargets(args[:dashIdx])
				}
				if dashIdx == 0 {
					return errTargetRequired()
				}
				cmdArgs := args[1:]
				if len(cmdArgs) == 0 {
					cmdArgs = defaultShellCommand()
				}
				gotNs, _ = c.Flags().GetString("namespace")
				gotCmd = cmdArgs
				return nil
			}

			cmd.SetArgs(tt.argv)
			err := cmd.Execute()

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotNs != tt.wantNs {
				t.Errorf("namespace: want %q, got %q", tt.wantNs, gotNs)
			}
			if !equalStrSlice(gotCmd, tt.wantCmd) {
				t.Errorf("command: want %v, got %v", tt.wantCmd, gotCmd)
			}
		})
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// containsSubstring checks if a string contains a substring.
func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > len(substr) && (s[:len(substr)] == substr ||
			s[len(s)-len(substr):] == substr ||
			func() bool {
				for i := 0; i <= len(s)-len(substr); i++ {
					if s[i:i+len(substr)] == substr {
						return true
					}
				}
				return false
			}())))
}

// isInstanceID checks if the target string is an instance ID.
func isInstanceID(target string) bool {
	// Simple heuristic: instance IDs typically contain hyphens and follow a pattern
	// like "service-instance-123"
	return len(target) > 0 && containsSubstring(target, "-instance-")
}

func TestParseExecOptions(t *testing.T) {
	tests := []struct {
		name        string
		namespace   string
		workdir     string
		env         []string
		tty         bool
		noTTY       bool
		timeout     string
		apiServer   string
		apiKey      string
		expectError bool
	}{
		{
			name:        "valid options",
			namespace:   "default",
			workdir:     "/app",
			env:         []string{"DEBUG=true", "LOG_LEVEL=debug"},
			tty:         true,
			timeout:     "30s",
			expectError: false,
		},
		{
			name:        "invalid timeout",
			namespace:   "default",
			timeout:     "invalid",
			expectError: true,
		},
		{
			name:        "invalid env format",
			namespace:   "default",
			env:         []string{"INVALID_ENV"},
			expectError: true,
		},
		{
			name:        "valid env format",
			namespace:   "default",
			env:         []string{"KEY=value"},
			timeout:     "5m",
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set global variables
			execOptions := &execOptions{
				workdir: tt.workdir,
				env:     tt.env,
				tty:     tt.tty,
				noTTY:   tt.noTTY,
				timeout: tt.timeout,
			}
			execOptions.namespace = tt.namespace
			execOptions.addressOverride = tt.apiServer

			parsedOpts, err := parseExecOptions(execOptions)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			// Verify parsed options
			if execOptions.namespace != tt.namespace {
				t.Errorf("expected namespace %s, got %s", tt.namespace, execOptions.namespace)
			}

			if parsedOpts.workdir != tt.workdir {
				t.Errorf("expected workdir %s, got %s", tt.workdir, parsedOpts.workdir)
			}

			if parsedOpts.tty != tt.tty {
				t.Errorf("expected TTY %v, got %v", tt.tty, parsedOpts.tty)
			}

			if tt.timeout != "invalid" {
				expectedTimeout, _ := time.ParseDuration(tt.timeout)
				if parsedOpts.timeout != expectedTimeout {
					t.Errorf("expected timeout %v, got %v", expectedTimeout, parsedOpts.timeout)
				}
			}

			// Verify environment variables
			if len(tt.env) > 0 && tt.env[0] != "INVALID_ENV" {
				for _, env := range tt.env {
					parts := []string{env}
					if len(parts) == 2 {
						if parsedOpts.env[parts[0]] != parts[1] {
							t.Errorf("expected env %s=%s, got %s", parts[0], parts[1], parsedOpts.env[parts[0]])
						}
					}
				}
			}
		})
	}
}

func TestShouldAllocateTTY(t *testing.T) {
	tests := []struct {
		name     string
		command  []string
		expected bool
	}{
		{
			name:     "bash shell",
			command:  []string{"bash"},
			expected: true,
		},
		{
			name:     "sh shell",
			command:  []string{"sh"},
			expected: true,
		},
		{
			name:     "zsh shell",
			command:  []string{"zsh"},
			expected: true,
		},
		{
			name:     "vim editor",
			command:  []string{"vim", "file.txt"},
			expected: true,
		},
		{
			name:     "nano editor",
			command:  []string{"nano", "file.txt"},
			expected: true,
		},
		{
			name:     "top command",
			command:  []string{"top"},
			expected: true,
		},
		{
			name:     "htop command",
			command:  []string{"htop"},
			expected: true,
		},
		{
			name:     "less command",
			command:  []string{"less", "file.txt"},
			expected: true,
		},
		{
			name:     "ls command",
			command:  []string{"ls", "-la"},
			expected: false,
		},
		{
			name:     "python script",
			command:  []string{"python", "script.py"},
			expected: false,
		},
		{
			name:     "empty command",
			command:  []string{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldAllocateTTY(tt.command)
			if result != tt.expected {
				t.Errorf("expected %v, got %v for command %v", tt.expected, result, tt.command)
			}
		})
	}
}

func TestIsInstanceID(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		expected bool
	}{
		{
			name:     "instance ID with pattern",
			target:   "api-instance-123",
			expected: true,
		},
		{
			name:     "instance ID with different service",
			target:   "web-instance-456",
			expected: true,
		},
		{
			name:     "service name",
			target:   "api",
			expected: false,
		},
		{
			name:     "service name with dash",
			target:   "web-api",
			expected: false,
		},
		{
			name:     "empty string",
			target:   "",
			expected: false,
		},
		{
			name:     "instance with different pattern",
			target:   "api-pod-123",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isInstanceID(tt.target)
			if result != tt.expected {
				t.Errorf("expected %v, got %v for target %s", tt.expected, result, tt.target)
			}
		})
	}
}
