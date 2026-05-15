//go:build unix

package portforwarddaemon

import (
	"os/exec"
	"syscall"
)

// setSidAttr detaches the spawned process from the parent's process
// group so the daemon survives the CLI exiting. Unix-only; Windows
// would need a different mechanism and is out of scope (RUNE-123).
func setSidAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
