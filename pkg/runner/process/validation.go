package process

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/runestack/rune/pkg/types"
)

// ValidateProcessSpec validates the process specification, including checking if the command exists
// and is executable on the system
func ValidateProcessSpec(spec *types.ProcessSpec) error {
	if spec == nil {
		return fmt.Errorf("process spec cannot be nil")
	}

	// Basic validation already done by spec.Validate()
	if err := spec.Validate(); err != nil {
		return err
	}

	// Validate that the command exists and is executable
	if err := validateExecutablePath(spec.Command); err != nil {
		return fmt.Errorf("command validation failed: %w", err)
	}

	return nil
}

// validateExecutablePath checks if the given command exists and is executable
func validateExecutablePath(command string) error {
	if command == "" {
		return fmt.Errorf("command cannot be empty")
	}

	// If command is an absolute path, check directly
	if filepath.IsAbs(command) {
		if err := checkFileExecutable(command); err != nil {
			return fmt.Errorf("absolute path executable validation failed: %w", err)
		}
		return nil
	}

	// If command contains path separators but isn't absolute,
	// it's a relative path - check in current directory
	if strings.ContainsRune(command, os.PathSeparator) {
		// Convert to absolute path based on current directory
		absPath, err := filepath.Abs(command)
		if err != nil {
			return fmt.Errorf("failed to convert to absolute path: %w", err)
		}

		if err := checkFileExecutable(absPath); err != nil {
			return fmt.Errorf("relative path executable validation failed: %w", err)
		}
		return nil
	}

	// Otherwise, check if command is in PATH
	_, err := exec.LookPath(command)
	if err != nil {
		return fmt.Errorf("command '%s' not found in PATH: %w", command, err)
	}

	return nil
}

// checkFileExecutable checks if a file exists and is executable
func checkFileExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file does not exist: %s", path)
		}
		return fmt.Errorf("failed to access file: %w", err)
	}

	// Check if it's a regular file (not a directory)
	if info.IsDir() {
		return fmt.Errorf("path is a directory, not an executable file: %s", path)
	}

	// Check if file is executable
	// This is platform-specific, but we can approximate
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("file is not executable: %s", path)
	}

	return nil
}
