package portforwarddaemon

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"
)

// EnsureRunning returns the path to the daemon socket, spawning the
// daemon if it isn't already running. If the daemon is up, this is a
// fast no-op (one ping round-trip).
//
// `daemonArgs` is the argv to pass to the spawned process; the caller
// supplies the path to the current binary plus the hidden subcommand
// (e.g. "__port-forward-daemon").
func EnsureRunning(stateDir string, daemonArgs []string) (string, error) {
	sockPath := SocketPath(stateDir)

	if isReachable(sockPath) {
		return sockPath, nil
	}

	if len(daemonArgs) == 0 {
		return "", errors.New("daemon args required to spawn")
	}

	// Open the log file for the daemon's stdout/stderr.
	logFile, err := os.OpenFile(LogPath(stateDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("open log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(daemonArgs[0], daemonArgs[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Detach: don't inherit our process group so the daemon survives
	// the CLI exiting. setsid via SysProcAttr.
	setSidAttr(cmd)

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("spawn daemon: %w", err)
	}
	// Don't Wait — we want it to outlive the CLI.

	// Poll for readiness.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if isReachable(sockPath) {
			return sockPath, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", fmt.Errorf("daemon did not become ready within timeout")
}

// isReachable pings the socket. Returns true iff a Ping round-trip
// succeeds.
func isReachable(sockPath string) bool {
	conn, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if err := WriteJSONLine(conn, Request{Cmd: CmdPing}); err != nil {
		return false
	}
	br := bufio.NewReader(conn)
	var resp Response
	if err := ReadJSONLine(br, &resp); err != nil {
		return false
	}
	return resp.OK
}

// Call sends one Request to the daemon socket and returns the Response.
func Call(sockPath string, req Request) (*Response, error) {
	conn, err := net.DialTimeout("unix", sockPath, 1*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := WriteJSONLine(conn, req); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	var resp Response
	if err := ReadJSONLine(br, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
