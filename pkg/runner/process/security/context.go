// Package security provides security implementations for process runners
package security

import (
	"fmt"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"syscall"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
)

// ApplySecurityContext applies security settings from a ProcessSecurityContext to a command
func ApplySecurityContext(cmd *exec.Cmd, sc *types.ProcessSecurityContext, logger log.Logger) error {
	if sc == nil {
		return nil // Nothing to apply
	}

	// Apply user/group if specified
	if sc.User != "" {
		if err := applyUserAndGroup(cmd, sc.User, sc.Group); err != nil {
			return fmt.Errorf("failed to apply user/group: %w", err)
		}
	}

	// Apply read-only filesystem if requested
	if sc.ReadOnlyFS {
		// This is platform-specific and would need more complex implementation
		// For now, we'll just log that it's requested but not fully implemented
		logger.Warn("Read-only filesystem requested but not fully implemented for process security context")
	}

	// Apply capabilities and syscall restrictions
	// These are Linux-specific features
	if runtime.GOOS == "linux" {
		if len(sc.Capabilities) > 0 || len(sc.AllowedSyscalls) > 0 || len(sc.DeniedSyscalls) > 0 {
			logger.Warn("Linux capabilities and syscall restrictions requested but not fully implemented")
		}
	}

	return nil
}

// applyUserAndGroup configures a command to run as the specified user and group
func applyUserAndGroup(cmd *exec.Cmd, username, groupname string) error {
	if username == "" {
		return nil
	}

	// Look up the user
	u, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("failed to look up user %s: %w", username, err)
	}

	// Convert user ID to integer
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("invalid user ID for %s: %w", username, err)
	}

	// Get group ID
	gid := -1
	if groupname != "" {
		// Use specified group
		g, err := user.LookupGroup(groupname)
		if err != nil {
			return fmt.Errorf("failed to look up group %s: %w", groupname, err)
		}
		gid, err = strconv.Atoi(g.Gid)
		if err != nil {
			return fmt.Errorf("invalid group ID for %s: %w", groupname, err)
		}
	} else if u.Gid != "" {
		// Use user's primary group
		gid, err = strconv.Atoi(u.Gid)
		if err != nil {
			return fmt.Errorf("invalid primary group ID for %s: %w", username, err)
		}
	}

	// Set credential in the command's SysProcAttr
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	// Apply credentials
	// This is OS-specific, for Unix-like systems
	if runtime.GOOS != "windows" {
		cmd.SysProcAttr.Credential = &syscall.Credential{
			Uid: utils.ToUint32NonNegative(uid),
			Gid: utils.ToUint32NonNegative(gid),
		}
	} else {
		return fmt.Errorf("user/group switching not supported on Windows")
	}

	return nil
}
