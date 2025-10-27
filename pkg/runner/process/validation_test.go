package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateProcessSpec(t *testing.T) {
	// Create a temporary directory for test files
	tempDir, err := os.MkdirTemp("", "process-validation-test")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	// Create a test executable file
	testExecPath := filepath.Join(tempDir, "test-executable")
	err = os.WriteFile(testExecPath, []byte("#!/bin/sh\necho Hello"), 0755)
	require.NoError(t, err)

	// Create a non-executable file
	nonExecPath := filepath.Join(tempDir, "non-executable")
	err = os.WriteFile(nonExecPath, []byte("data"), 0644)
	require.NoError(t, err)

	// Test cases
	testCases := []struct {
		name        string
		spec        *types.ProcessSpec
		expectError bool
		errorMsg    string
	}{
		{
			name: "Valid command in PATH",
			spec: &types.ProcessSpec{
				Command: "ls", // Should be available in most testing environments
				Args:    []string{"-la"},
			},
			expectError: false,
		},
		{
			name: "Valid command with absolute path",
			spec: &types.ProcessSpec{
				Command: testExecPath,
				Args:    []string{"arg1", "arg2"},
			},
			expectError: false,
		},
		{
			name: "Empty command",
			spec: &types.ProcessSpec{
				Command: "",
				Args:    []string{},
			},
			expectError: true,
			errorMsg:    "command is required",
		},
		{
			name: "Non-existent command",
			spec: &types.ProcessSpec{
				Command: "this-command-should-not-exist-12345",
				Args:    []string{},
			},
			expectError: true,
			errorMsg:    "not found in PATH",
		},
		{
			name: "Non-executable file",
			spec: &types.ProcessSpec{
				Command: nonExecPath,
				Args:    []string{},
			},
			expectError: true,
			errorMsg:    "not executable",
		},
		{
			name:        "Nil spec",
			spec:        nil,
			expectError: true,
			errorMsg:    "cannot be nil",
		},
	}

	// Run test cases
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateProcessSpec(tc.spec)

			if tc.expectError {
				assert.Error(t, err)
				if tc.errorMsg != "" {
					assert.Contains(t, err.Error(), tc.errorMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateExecutablePath(t *testing.T) {
	// Find a command that should exist in PATH
	lsPath, err := exec.LookPath("ls")
	require.NoError(t, err, "Test requires 'ls' command to be in PATH")

	// Test validating a command in PATH
	err = validateExecutablePath("ls")
	assert.NoError(t, err)

	// Test validating with absolute path
	err = validateExecutablePath(lsPath)
	assert.NoError(t, err)

	// Test with empty command
	err = validateExecutablePath("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty")

	// Test with non-existent command
	err = validateExecutablePath("this-command-should-not-exist-12345")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found in PATH")
}

func TestCheckFileExecutable(t *testing.T) {
	// Create a temporary directory for test files
	tempDir, err := os.MkdirTemp("", "file-executable-test")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	// Create a test executable file
	execPath := filepath.Join(tempDir, "executable")
	err = os.WriteFile(execPath, []byte("#!/bin/sh\necho test"), 0755)
	require.NoError(t, err)

	// Create a non-executable file
	nonExecPath := filepath.Join(tempDir, "non-executable")
	err = os.WriteFile(nonExecPath, []byte("data"), 0644)
	require.NoError(t, err)

	// Test cases
	testCases := []struct {
		name        string
		path        string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "Executable file",
			path:        execPath,
			expectError: false,
		},
		{
			name:        "Non-executable file",
			path:        nonExecPath,
			expectError: true,
			errorMsg:    "not executable",
		},
		{
			name:        "Directory",
			path:        tempDir,
			expectError: true,
			errorMsg:    "directory",
		},
		{
			name:        "Non-existent file",
			path:        filepath.Join(tempDir, "does-not-exist"),
			expectError: true,
			errorMsg:    "does not exist",
		},
	}

	// Run test cases
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkFileExecutable(tc.path)

			if tc.expectError {
				assert.Error(t, err)
				if tc.errorMsg != "" {
					assert.Contains(t, err.Error(), tc.errorMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
