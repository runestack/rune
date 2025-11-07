package watcher

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/types"
)

// getTerminalSize returns the dimensions of the terminal
func getTerminalSize() (width, height int, err error) {
	// Default sizes if detection fails
	width, height = 80, 24

	// Try to get actual terminal size
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err == nil {
		parts := strings.Split(strings.TrimSpace(string(out)), " ")
		if len(parts) == 2 {
			if h, err := strconv.Atoi(parts[0]); err == nil {
				height = h
			}
			if w, err := strconv.Atoi(parts[1]); err == nil {
				width = w
			}
		}
	}

	return width, height, err
}

// formatAge formats a time.Time as a human-readable age string
func formatAge(t time.Time) string {
	if t.IsZero() {
		return "Unknown"
	}

	duration := time.Since(t)
	if duration < time.Minute {
		return "Just now"
	} else if duration < time.Hour {
		minutes := int(duration.Minutes())
		return fmt.Sprintf("%dm", minutes)
	} else if duration < 24*time.Hour {
		hours := int(duration.Hours())
		return fmt.Sprintf("%dh", hours)
	} else if duration < 30*24*time.Hour {
		days := int(duration.Hours() / 24)
		return fmt.Sprintf("%dd", days)
	} else if duration < 365*24*time.Hour {
		months := int(duration.Hours() / 24 / 30)
		return fmt.Sprintf("%dmo", months)
	}
	years := int(duration.Hours() / 24 / 365)
	return fmt.Sprintf("%dy", years)
}

// truncateString truncates a string to maxLen and adds "..." if it was truncated
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// countRunningInstances counts running instances for a service
// This is a placeholder that should be replaced with actual instance counting logic
func countRunningInstances(service *types.Service) int {
	// This is a placeholder - in a real implementation, we would count
	// the actual running instances from the service's status
	if service.Status == types.ServiceStatusRunning {
		return service.Scale
	}
	return 0
}
