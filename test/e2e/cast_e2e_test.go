//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apiclient "github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestCastService_EndToEnd(t *testing.T) {
	// Build binaries (runed-test and rune)
	cwd, _ := os.Getwd()
	projectRoot := findProjectRoot(cwd)
	if projectRoot == "" {
		t.Fatalf("failed to determine project root from %s", cwd)
	}

	runedTest := filepath.Join(projectRoot, "bin", "runed-test")
	if _, err := os.Stat(runedTest); err != nil {
		cmd := exec.Command("go", "build", "-o", runedTest, "./cmd/runed-test")
		cmd.Dir = projectRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("failed to build runed-test: %v\n%s", err, string(out))
		}
	}

	runeCLI := filepath.Join(projectRoot, "bin", "rune")
	if _, err := os.Stat(runeCLI); err != nil {
		cmd := exec.Command("go", "build", "-o", runeCLI, "./cmd/rune")
		cmd.Dir = projectRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("failed to build rune CLI: %v\n%s", err, string(out))
		}
	}

	grpcAddr := "127.0.0.1:19080"
	httpAddr := "127.0.0.1:19081"

	// Start server
	srvCmd := exec.Command(runedTest, "--grpc", grpcAddr, "--http", httpAddr)
	srvCmd.Env = append(os.Environ(), "RUNE_LOG_LEVEL=error")
	if err := srvCmd.Start(); err != nil {
		t.Fatalf("failed to start runed-test: %v", err)
	}
	defer func() {
		_ = srvCmd.Process.Kill()
		_, _ = srvCmd.Process.Wait()
	}()

	// Wait for gRPC health
	timeout := 30 * time.Second
	if s := os.Getenv("RUNE_E2E_HEALTH_TIMEOUT_SECONDS"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			timeout = time.Duration(v) * time.Second
		}
	}
	if err := waitForGRPCHealth(grpcAddr, timeout); err != nil {
		t.Fatalf("server not healthy: %v", err)
	}

	// Create a temp service file
	tempDir := t.TempDir()
	svcFile := filepath.Join(tempDir, "nginx.yaml")
	if err := os.WriteFile(svcFile, []byte(`
service:
  name: nginx-e2e
  image: nginx:latest
  scale: 1
`), 0644); err != nil {
		t.Fatalf("failed to write service file: %v", err)
	}

	// Run rune cast against the server
	castCmd := exec.Command(runeCLI, "cast", "--api-server", grpcAddr, svcFile)
	castCmd.Env = append(os.Environ(), "RUNE_LOG_LEVEL=error")
	out, err := castCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rune cast failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "Successfully deployed") {
		t.Logf("cast output: %s", string(out))
	}

	// Verify via API
	cli, err := apiclient.NewClient(&apiclient.ClientOptions{Address: grpcAddr})
	if err != nil {
		t.Fatalf("failed to create api client: %v", err)
	}
	defer cli.Close()

	svcClient := generated.NewServiceServiceClient(cli.Conn())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := svcClient.GetService(ctx, &generated.GetServiceRequest{Name: "nginx-e2e", Namespace: "default"})
	if err != nil {
		t.Fatalf("failed to get service via api: %v", err)
	}
	if resp == nil || resp.Service == nil || resp.Service.Name != "nginx-e2e" {
		t.Fatalf("unexpected service response: %#v", resp)
	}
}

func waitForHTTPHealth(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) // #nosec G107 - local test address
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", url)
}

func waitForGRPCHealth(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		if err == nil {
			client := generated.NewHealthServiceClient(conn)
			if _, herr := client.GetHealth(ctx, &generated.GetHealthRequest{}); herr == nil {
				_ = conn.Close()
				cancel()
				return nil
			}
			_ = conn.Close()
		}
		cancel()
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for gRPC health at %s", addr)
}

func findProjectRoot(start string) string {
	// Walk up until we find go.mod
	d := start
	for i := 0; i < 6; i++ { // up to 6 levels
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		d = filepath.Dir(d)
	}
	return ""
}
