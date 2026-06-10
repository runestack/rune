//go:build e2e
// +build e2e

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const cliTimeout = 60 * time.Second

// CLI runs the real `rune` binary against the test's server. The
// server address and bearer token are injected through a per-test CLI
// config file (via RUNE_CLI_CONFIG), so commands run exactly as a user
// would type them — no extra flags.
type CLI struct {
	bin        string
	configPath string
}

// Result captures one CLI invocation.
type Result struct {
	Args     []string
	ExitCode int
	Stdout   string
	Stderr   string
}

// Succeeded reports a zero exit code.
func (r Result) Succeeded() bool { return r.ExitCode == 0 }

// StdoutContains reports whether stdout contains needle.
func (r Result) StdoutContains(needle string) bool { return strings.Contains(r.Stdout, needle) }

// StderrContains reports whether stderr contains needle.
func (r Result) StderrContains(needle string) bool { return strings.Contains(r.Stderr, needle) }

// Contains reports whether either stream contains needle.
func (r Result) Contains(needle string) bool {
	return r.StdoutContains(needle) || r.StderrContains(needle)
}

// String renders the invocation for failure messages.
func (r Result) String() string {
	return fmt.Sprintf("rune %s (exit %d)\n--- stdout ---\n%s\n--- stderr ---\n%s",
		strings.Join(r.Args, " "), r.ExitCode, r.Stdout, r.Stderr)
}

func newCLI(t *testing.T, bin, serverAddr string) *CLI {
	t.Helper()
	c := &CLI{bin: bin, configPath: t.TempDir() + "/cli-config.yaml"}
	c.writeConfig(t, serverAddr, "")
	return c
}

// setToken rewrites the CLI config with the bearer token. Reading the
// address back from the file keeps a single source of truth.
func (c *CLI) writeConfig(t *testing.T, serverAddr, token string) {
	t.Helper()
	cfg := fmt.Sprintf("current-context: default\ncontexts:\n  default:\n    server: %s\n", serverAddr)
	if token != "" {
		cfg += fmt.Sprintf("    token: %s\n", token)
	}
	if err := os.WriteFile(c.configPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("harness: write CLI config: %v", err)
	}
}

// Run executes `rune args...` and returns the result. It fails the
// test only on harness-level errors (binary missing, timeout); a
// non-zero exit from the CLI is data for the caller.
func (c *CLI) Run(t *testing.T, args ...string) Result {
	t.Helper()
	cmd := exec.Command(c.bin, args...)
	cmd.Env = append(os.Environ(),
		"RUNE_CLI_CONFIG="+c.configPath,
		// Plain output: no ANSI color tables to fight in assertions.
		"NO_COLOR=1",
		"TERM=dumb",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("harness: start rune %v: %v", args, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var exitCode int
	select {
	case err := <-done:
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
			} else {
				t.Fatalf("harness: rune %v: %v", args, err)
			}
		}
	case <-time.After(cliTimeout):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("harness: rune %v timed out after %s\n--- stdout ---\n%s\n--- stderr ---\n%s",
			args, cliTimeout, stdout.String(), stderr.String())
	}

	return Result{Args: args, ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()}
}

// MustRun executes the command and fails the test unless it exits 0.
func (c *CLI) MustRun(t *testing.T, args ...string) Result {
	t.Helper()
	res := c.Run(t, args...)
	if !res.Succeeded() {
		t.Fatalf("harness: command failed: %s", res)
	}
	return res
}
