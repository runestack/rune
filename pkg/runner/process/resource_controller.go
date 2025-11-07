package process

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/runestack/rune/pkg/types"
)

// resourceController handles resource limits for a process.
type resourceController struct {
	pid        int              // Process ID
	cgroupPath string           // Path to cgroup directory
	resources  *types.Resources // Resource limits
}

// newResourceController creates a new resource controller for a process.
func newResourceController(pid int, resources *types.Resources) (*resourceController, error) {
	// This would use OS-specific mechanisms
	// On Linux, this would use cgroups
	// On other platforms, we might have more limited functionality

	// For now, we'll only support Linux cgroups v2
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("resource control is only supported on Linux")
	}

	// Check if cgroups v2 is available
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return nil, fmt.Errorf("cgroups v2 not available on this system")
	}

	// Create cgroup path
	cgroupPath := fmt.Sprintf("/sys/fs/cgroup/rune/process-%d", pid)

	// Create cgroup directory
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cgroup directory: %w", err)
	}

	ctrl := &resourceController{
		pid:        pid,
		cgroupPath: cgroupPath,
		resources:  resources,
	}

	// Apply CPU limits
	if resources != nil && resources.CPU.Limit != "" || resources.CPU.Request != "" {
		limit := resources.CPU.Limit
		if limit == "" {
			limit = resources.CPU.Request
		}

		// Parse and apply the CPU limit
		if err := ctrl.applyCPULimit(limit); err != nil {
			// Log error but continue
			return ctrl, err
		}
	}

	// Apply memory limits
	if resources != nil && resources.Memory.Limit != "" || resources.Memory.Request != "" {
		limit := resources.Memory.Limit
		if limit == "" {
			limit = resources.Memory.Request
		}

		// Parse and apply the memory limit
		if err := ctrl.applyMemoryLimit(limit); err != nil {
			// Log error but continue
			return ctrl, err
		}
	}

	// Add process to cgroup
	if err := ctrl.addProcess(); err != nil {
		return ctrl, err
	}

	return ctrl, nil
}

// applyCPULimit applies CPU limits to the cgroup.
func (c *resourceController) applyCPULimit(cpuLimit string) error {
	// Parse CPU limit using the helper function
	cpuValue, err := types.ParseCPU(cpuLimit)
	if err != nil {
		return fmt.Errorf("invalid CPU limit format: %w", err)
	}

	// For cgroups v2, we'd write to cpu.max
	cpuMaxPath := filepath.Join(c.cgroupPath, "cpu.max")

	// Convert CPU cores to the appropriate format
	// cgroups v2 uses a quota/period format
	// For 1 CPU core, quota = period (100% of 1 CPU)
	// For 0.5 CPU cores, quota = period/2 (50% of 1 CPU)
	period := 100000
	quota := int(cpuValue * float64(period))
	value := fmt.Sprintf("%d %d", quota, period)

	if err := os.WriteFile(cpuMaxPath, []byte(value), 0o600); err != nil {
		return fmt.Errorf("failed to write CPU limit: %w", err)
	}

	return nil
}

// applyMemoryLimit applies memory limits to the cgroup.
func (c *resourceController) applyMemoryLimit(memoryLimit string) error {
	// Parse memory limit using the helper function
	memoryBytes, err := types.ParseMemory(memoryLimit)
	if err != nil {
		return fmt.Errorf("invalid memory limit format: %w", err)
	}

	// For cgroups v2, we'd write to memory.max
	memoryMaxPath := filepath.Join(c.cgroupPath, "memory.max")

	// Write the memory limit in bytes
	if err := os.WriteFile(memoryMaxPath, []byte(strconv.FormatInt(memoryBytes, 10)), 0o600); err != nil {
		return fmt.Errorf("failed to write memory limit: %w", err)
	}

	return nil
}

// addProcess adds the process to the cgroup.
func (c *resourceController) addProcess() error {
	// For cgroups v2, we'd write the PID to cgroup.procs
	procsPath := filepath.Join(c.cgroupPath, "cgroup.procs")

	// Write the PID
	if err := os.WriteFile(procsPath, []byte(strconv.Itoa(c.pid)), 0o600); err != nil {
		return fmt.Errorf("failed to add process to cgroup: %w", err)
	}

	return nil
}

// cleanup removes the cgroup.
func (c *resourceController) cleanup() error {
	// For cgroups v2, we can simply remove the directory
	if err := os.RemoveAll(c.cgroupPath); err != nil {
		return fmt.Errorf("failed to remove cgroup: %w", err)
	}

	return nil
}
