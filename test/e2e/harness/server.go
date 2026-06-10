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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	defaultReadyTimeout = 60 * time.Second
	readyPollInterval   = 100 * time.Millisecond
	shutdownGrace       = 5 * time.Second
)

// Server is a real runed process owned by a single test.
type Server struct {
	GRPCAddr string
	HTTPAddr string
	DataDir  string
	LogPath  string

	cmd     *exec.Cmd
	logFile *os.File
	stopped bool

	// waitDone closes once the single Wait goroutine reaps the
	// process; waitErr is valid after that. One reaper avoids the
	// Wait-called-twice trap between readiness checks and Stop.
	waitDone chan struct{}
	waitErr  error
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

	runefilePath := dir + "/runefile.toml"
	runefile := renderRunefile(opts, dataDir, s.GRPCAddr, s.HTTPAddr)
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
	// Hermetic environment: the user's shell may carry RUNE_*
	// overrides (RUNE_SERVER_GRPC_ADDRESS, RUNE_LOG_LEVEL, …) that
	// viper would silently honor over the generated runefile.
	cmd.Env = scrubRuneEnv(os.Environ())
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
	s.waitDone = make(chan struct{})
	go func() {
		s.waitErr = cmd.Wait()
		close(s.waitDone)
	}()

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
// just that the socket is bound. One lazy connection is reused across
// polls; gRPC redials it as the server comes up.
func (s *Server) waitReady(timeout time.Duration) error {
	conn, err := grpc.NewClient(s.GRPCAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("create probe client: %w", err)
	}
	defer conn.Close()
	health := generated.NewHealthServiceClient(conn)

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-s.waitDone:
			return fmt.Errorf("runed exited before becoming ready: %v", s.waitErr)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, herr := health.GetHealth(ctx, &generated.GetHealthRequest{})
		cancel()
		// Unauthenticated still proves the server is up and serving;
		// the bootstrap token doesn't exist yet at this point.
		if herr == nil || status.Code(herr) == codes.Unauthenticated {
			return nil
		}
		lastErr = herr
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

	select {
	case <-s.waitDone:
	case <-time.After(shutdownGrace):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-s.waitDone
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
