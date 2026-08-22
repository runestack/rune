//go:build e2e
// +build e2e

package harness

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	apiclient "github.com/runestack/rune/pkg/api/client"
	"google.golang.org/grpc"
)

// DefaultConvergeTimeout is a sensible Eventually bound for
// control-plane state changes (orchestrator reconciliation, deletes).
const DefaultConvergeTimeout = 30 * time.Second

// Context owns one running runed instance plus authenticated clients
// for it. Everything is torn down via t.Cleanup; tests never manage
// lifecycle by hand.
type Context struct {
	Server *Server
	CLI    *CLI

	// Token is the bootstrapped server-admin bearer, already wired
	// into CLI and the SDK client.
	Token string

	t      *testing.T
	client *apiclient.Client
}

// New builds the binaries (once per process), starts an isolated
// dev-mode runed, bootstraps the admin token, and returns a Context
// ready for CLI, gRPC, and HTTP traffic.
func New(t *testing.T, options ...Option) *Context {
	t.Helper()
	var opts Options
	for _, o := range options {
		o(&opts)
	}

	server := startServer(t, opts)
	_, runeCLI := binaries(t)
	cli := newCLI(t, runeCLI, server.GRPCAddr)

	// First-run bootstrap mints the server-admin grant; stdout is the
	// bare token.
	res := cli.MustRun(t, "admin", "bootstrap")
	token := strings.TrimSpace(res.Stdout)
	if token == "" {
		t.Fatalf("harness: admin bootstrap returned empty token: %s", res)
	}
	cli.writeConfig(t, server.GRPCAddr, token)

	ctx := &Context{Server: server, CLI: cli, Token: token, t: t}
	t.Cleanup(func() {
		if ctx.client != nil {
			_ = ctx.client.Close()
		}
	})
	return ctx
}

// Client returns the SDK client (lazily dialed, authenticated as the
// bootstrapped admin).
func (c *Context) Client() *apiclient.Client {
	c.t.Helper()
	if c.client == nil {
		cli, err := apiclient.NewClient(&apiclient.ClientOptions{
			Address:     c.Server.GRPCAddr,
			Token:       c.Token,
			DialTimeout: 15 * time.Second,
			CallTimeout: 30 * time.Second,
		})
		if err != nil {
			c.t.Fatalf("harness: dial api client: %v", err)
		}
		c.client = cli
	}
	return c.client
}

// Conn returns the raw gRPC connection for generated service clients.
func (c *Context) Conn() *grpc.ClientConn { return c.Client().Conn() }

// Ctx returns a context bounded to a sensible per-call deadline.
func (c *Context) Ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// HTTPURL joins path onto the server's HTTP listener.
func (c *Context) HTTPURL(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return fmt.Sprintf("http://%s%s", c.Server.HTTPAddr, path)
}

// HTTPGet issues an authenticated GET against the HTTP listener.
func (c *Context) HTTPGet(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.HTTPURL(path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	client := &http.Client{
		Timeout: 15 * time.Second,
		// Surface redirects (e.g. / → /ui) to the test instead of
		// silently following them.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client.Do(req)
}

// LogsContain reports whether the server log contains needle.
func (c *Context) LogsContain(needle string) bool { return c.Server.LogsContain(needle) }

// Eventually polls fn until it returns true or the timeout elapses,
// then fails the test. Use for state that converges asynchronously
// (orchestrator reconciliation, instance status) instead of sleeps.
func (c *Context) Eventually(timeout time.Duration, what string, fn func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	c.t.Fatalf("harness: timed out after %s waiting for %s", timeout, what)
}

// CLIAs returns a second CLI bound to the same server but authenticated
// with the given bearer token. Used by tests that need to exercise a
// narrower credential (e.g. a `--permissions cast` service account)
// alongside the bootstrapped admin.
func (c *Context) CLIAs(t *testing.T, token string) *CLI {
	t.Helper()
	cli := newCLI(t, c.CLI.bin, c.Server.GRPCAddr)
	cli.writeConfig(t, c.Server.GRPCAddr, token)
	return cli
}
