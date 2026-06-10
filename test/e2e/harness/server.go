//go:build e2e
// +build e2e

package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultReadyTimeout = 60 * time.Second
	readyPollInterval   = 100 * time.Millisecond
	shutdownGrace       = 5 * time.Second
)

// Server is a real runed process owned by a single test.
type Server struct {
	GRPCAddr    string
	HTTPAddr    string
	MetricsAddr string // empty unless Options.Metrics
	DataDir     string
	LogPath     string

	cmd     *exec.Cmd
	logFile *os.File
	stopped bool
}

// startServer builds (once), configures and spawns runed, then blocks
// until the gRPC HealthService answers. Cleanup is registered on t.
func startServer(t *testing.T, opts Options) *Server {
	t.Helper()
	runed, _ := binaries(t)

	dir := t.TempDir()
	dataDir := dir + "/data"
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("harness: create data dir: %v", err)
	}

	s := &Server{
		GRPCAddr: freeAddr(t),
		HTTPAddr: freeAddr(t),
		DataDir:  dataDir,
		LogPath:  dir + "/runed.log",
	}
	if opts.Metrics {
		s.MetricsAddr = freeAddr(t)
	}

	runefilePath := dir + "/runefile.toml"
	runefile := renderRunefile(opts, dataDir, s.GRPCAddr, s.HTTPAddr, s.MetricsAddr)
	if err := os.WriteFile(runefilePath, []byte(runefile), 0o644); err != nil {
		t.Fatalf("harness: write runefile: %v", err)
	}

	logFile, err := os.Create(s.LogPath)
	if err != nil {
		t.Fatalf("harness: create log file: %v", err)
	}
	s.logFile = logFile

	cmd := exec.Command(runed, "--config", runefilePath, "--dev-mode")
	cmd.Dir = dir
	// Handing the file straight to the child avoids pipe-drain
	// goroutines entirely; the OS does the teeing.
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Own process group so Stop can kill runed plus anything it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("harness: start runed: %v", err)
	}
	s.cmd = cmd

	t.Cleanup(func() {
		s.Stop()
		if t.Failed() {
			s.dumpLogTail(t)
		}
	})

	if err := s.waitReady(readyTimeout()); err != nil {
		// Cleanup will dump the log; fail with the proximate cause.
		t.Fatalf("harness: runed not ready: %v", err)
	}
	return s
}

// readyTimeout honors RUNE_E2E_HEALTH_TIMEOUT_SECONDS (shared with the
// previous E2E setup) so slow CI runners can stretch the window.
func readyTimeout() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("RUNE_E2E_HEALTH_TIMEOUT_SECONDS")); err == nil && v > 0 {
		return time.Duration(v) * time.Second
	}
	return defaultReadyTimeout
}

// waitReady polls the gRPC HealthService until it answers — stronger
// than a TCP probe because it proves the API layer is serving, not
// just that the socket is bound.
func (s *Server) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if s.cmd.ProcessState != nil {
			return fmt.Errorf("runed exited early: %v", s.cmd.ProcessState)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := grpc.DialContext(ctx, s.GRPCAddr,
			grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		if err == nil {
			_, herr := generated.NewHealthServiceClient(conn).GetHealth(ctx, &generated.GetHealthRequest{})
			conn.Close()
			cancel()
			// Unauthenticated still proves the server is up and serving.
			if herr == nil || strings.Contains(herr.Error(), "Unauthenticated") {
				return nil
			}
			lastErr = herr
		} else {
			lastErr = err
			cancel()
		}
		time.Sleep(readyPollInterval)
	}
	return fmt.Errorf("timed out after %s waiting for %s (last error: %v)", timeout, s.GRPCAddr, lastErr)
}

// Stop terminates the server: SIGTERM to the process group, a grace
// period for clean badger shutdown, then SIGKILL. Idempotent.
func (s *Server) Stop() {
	if s.stopped || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	s.stopped = true

	pgid := -s.cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		_, _ = s.cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-done
	}
	if s.logFile != nil {
		_ = s.logFile.Close()
	}
}

// Logs returns everything the server has written so far.
func (s *Server) Logs() (string, error) {
	b, err := os.ReadFile(s.LogPath)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// LogsContain reports whether the captured server log contains needle.
func (s *Server) LogsContain(needle string) bool {
	logs, err := s.Logs()
	return err == nil && strings.Contains(logs, needle)
}

// dumpLogTail prints the end of the server log into the test output so
// a red CI run carries the server's side of the story. Capped by bytes,
// not lines — the JSON formatter emits entries without newlines, so the
// whole log can be one enormous physical line.
func (s *Server) dumpLogTail(t *testing.T) {
	const maxBytes = 16 * 1024
	logs, err := s.Logs()
	if err != nil {
		t.Logf("harness: could not read server log: %v", err)
		return
	}
	tail := logs
	if len(tail) > maxBytes {
		tail = "…" + tail[len(tail)-maxBytes:]
	}
	t.Logf("harness: tail of runed log (%s):\n%s", s.LogPath, tail)
}
