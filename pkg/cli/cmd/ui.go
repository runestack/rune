package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newUICmd() *cobra.Command {
	var uiURL string
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "ui",
		Args:  cobra.NoArgs,
		Short: "Open the Rune dashboard in your browser, signed in",
		Long: `Open the embedded Rune dashboard in your browser, signed in with your
current CLI session (RUNE-201). The CLI hands a one-time code to the server,
which mints a fresh browser-scoped session as an HttpOnly cookie — your CLI
token is never exposed to the browser.

By default the dashboard is tunnelled over the authenticated control-plane
connection and opened on a local address: no exposed port, no SSH, and it
satisfies the server's default require-TLS (the browser hits loopback). The
command stays running while you use the dashboard — press Ctrl-C to stop.

Use --url to instead hand off directly to an already-reachable dashboard URL
(e.g. a TLS-fronted production deployment); that form exits immediately.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if viper.GetString("contexts.default.token") == "" &&
				viper.GetString("contexts.default.refreshToken") == "" {
				if _, ok := getEnv("RUNE_TOKEN"); !ok {
					return fmt.Errorf("no active session; run 'rune login' first")
				}
			}

			// Refresh the access token before the handoff POST: the gRPC client
			// transparently refreshes on Unauthenticated and persists the rotated
			// tokens. Without this, an expired access token would 401 the handoff
			// even with a valid refresh grant.
			if err := ensureFreshSession(); err != nil {
				return err
			}
			token := currentBearerToken()
			if token == "" {
				return fmt.Errorf("no usable access token after refresh; run 'rune login' again")
			}

			if uiURL != "" {
				return runUIDirect(cmd.Context(), strings.TrimRight(uiURL, "/"), token, noOpen)
			}
			return runUITunnel(cmd.Context(), token, noOpen)
		},
	}
	cmd.Flags().StringVar(&uiURL, "url", "", "Reachable dashboard base URL (e.g. https://rune.example.com); skips the tunnel and does a one-shot handoff")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Print the sign-in URL without opening a browser")
	return cmd
}

// performHandoff mints a one-time code, POSTs the bearer under it to the
// dashboard's /v1/ui/handoff endpoint, and returns the browser sign-in URL.
func performHandoff(ctx context.Context, base, token string) (string, error) {
	code, err := randomHandoffCode()
	if err != nil {
		return "", fmt.Errorf("generate handoff code: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/ui/handoff/"+code, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to reach dashboard at %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return "", fmt.Errorf("handoff failed (status %d); is the dashboard enabled on the server?", resp.StatusCode)
	}
	return base + "/ui/login?handoff=" + code, nil
}

// runUIDirect signs in against an already-reachable dashboard URL and exits.
func runUIDirect(ctx context.Context, base, token string, noOpen bool) error {
	loginURL, err := performHandoff(ctx, base, token)
	if err != nil {
		return err
	}
	announceSignIn(loginURL, noOpen)
	return nil
}

// runUITunnel forwards runed's own dashboard HTTP listener over the
// authenticated control-plane stream, opens it on a loopback port, and stays
// running until the context is cancelled (Ctrl-C).
func runUITunnel(ctx context.Context, token string, noOpen bool) error {
	apiClient, err := newAPIClient("", "")
	if err != nil {
		return err
	}
	defer apiClient.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			fmt.Fprintln(os.Stderr, "rune ui: stopping")
			cancel()
		case <-ctx.Done():
		}
	}()

	pf := client.NewPortForwardClient(apiClient)
	sess, _, err := pf.Open(ctx, client.PortForwardTarget{
		ControlPlane: true,
		Ports:        []uint32{uint32(config.DefaultHTTPPort)}, //nolint:gosec // G115: DefaultHTTPPort is a small constant
	})
	if err != nil {
		return fmt.Errorf("open dashboard tunnel (is the dashboard enabled on the server?): %w", err)
	}
	defer sess.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind local tunnel port: %w", err)
	}
	defer ln.Close()
	localPort := ln.Addr().(*net.TCPAddr).Port

	// Demultiplex server frames into per-conn channels.
	router := newConnRouter()
	recvDone := make(chan error, 1)
	go func() {
		for {
			msg, rerr := sess.Recv()
			if rerr != nil {
				recvDone <- rerr
				return
			}
			router.dispatch(msg)
		}
	}()

	// Accept local browser connections; the server ignores remote_port for
	// control_plane and dials its own HTTP listener.
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go handleLocalConn(ctx, sess, router, c, uint16(config.DefaultHTTPPort)) //nolint:gosec // G115: DefaultHTTPPort is a small constant
		}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", localPort)
	loginURL, err := performHandoff(ctx, base, token)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Dashboard tunnel ready at %s/ui\n", base)
	announceSignIn(loginURL, noOpen)
	fmt.Fprintln(os.Stderr, "Tunnel active — press Ctrl-C to stop.")

	select {
	case <-ctx.Done():
		return nil
	case err := <-recvDone:
		if err != nil && err != io.EOF {
			return fmt.Errorf("dashboard tunnel closed: %w", err)
		}
		return nil
	}
}

func announceSignIn(loginURL string, noOpen bool) {
	fmt.Printf("Open this URL to finish signing in:\n  %s\n", loginURL)
	if noOpen {
		return
	}
	if err := openBrowser(loginURL); err != nil {
		fmt.Printf("(could not open a browser automatically: %v)\n", err)
	}
}

// ensureFreshSession makes one authenticated gRPC call (WhoAmI) through the
// standard client, which transparently refreshes an expired access token using
// the stored refresh grant and persists the rotated tokens to the context file.
func ensureFreshSession() error {
	api, err := newAPIClient("", "")
	if err != nil {
		return err
	}
	defer api.Close()
	ac := generated.NewAuthServiceClient(api.Conn())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ac.WhoAmI(ctx, &generated.WhoAmIRequest{}); err != nil {
		return fmt.Errorf("current session is not valid (run 'rune login'): %w", err)
	}
	return nil
}

// currentBearerToken reads the freshest access token from the context file
// (which ensureFreshSession may have just rewritten), falling back to viper/env.
func currentBearerToken() string {
	if cfg, err := loadContextConfig(); err == nil {
		if c, ok := cfg.Contexts[cfg.CurrentContext]; ok && c.Token != "" {
			return c.Token
		}
	}
	if t := viper.GetString("contexts.default.token"); t != "" {
		return t
	}
	if t, ok := getEnv("RUNE_TOKEN"); ok {
		return t
	}
	return ""
}

func randomHandoffCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// openBrowser best-effort opens a URL in the platform's default browser.
func openBrowser(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Start()
	default:
		return exec.Command("xdg-open", target).Start()
	}
}
