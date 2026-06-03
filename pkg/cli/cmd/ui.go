package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/api/generated"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newUICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Dashboard helpers (browser sign-in)",
	}
	cmd.AddCommand(newUILoginCmd())
	return cmd
}

func newUILoginCmd() *cobra.Command {
	var uiURL string
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to the dashboard in your browser using the current CLI session",
		Long: `Open the Rune dashboard in your browser, signed in with your current CLI
session (RUNE-201). The CLI hands a one-time code to the server, which mints a
fresh browser-scoped session delivered as an HttpOnly cookie — your CLI token is
never exposed to the browser.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if viper.GetString("contexts.default.token") == "" &&
				viper.GetString("contexts.default.refreshToken") == "" {
				if _, ok := getEnv("RUNE_TOKEN"); !ok {
					return fmt.Errorf("no active session; run 'rune login' first")
				}
			}

			// Ensure a valid (non-expired) access token before the handoff POST:
			// the gRPC client transparently refreshes on Unauthenticated and
			// persists the rotated tokens to the context. Without this, an expired
			// access token would 401 the handoff even with a valid refresh grant.
			if err := ensureFreshSession(); err != nil {
				return err
			}
			token := currentBearerToken()
			if token == "" {
				return fmt.Errorf("no usable access token after refresh; run 'rune login' again")
			}

			base, err := resolveUIBaseURL(uiURL)
			if err != nil {
				return err
			}

			// One-time handoff code minted client-side; the CLI POSTs its bearer
			// under it, the browser claims it.
			code, err := randomHandoffCode()
			if err != nil {
				return fmt.Errorf("generate handoff code: %w", err)
			}

			req, err := http.NewRequest(http.MethodPost, base+"/v1/ui/handoff/"+code, nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+token)
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("failed to reach dashboard at %s: %w", base, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				return fmt.Errorf("handoff failed (status %d); is the dashboard enabled on the server?", resp.StatusCode)
			}

			loginURL := base + "/ui/login?handoff=" + code
			fmt.Printf("Open this URL to finish signing in:\n  %s\n", loginURL)
			if !noOpen {
				if err := openBrowser(loginURL); err != nil {
					fmt.Printf("(could not open a browser automatically: %v)\n", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&uiURL, "ui-url", "", "Dashboard base URL (e.g. https://rune.example.com). Defaults to the context server host on the HTTP port")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Print the sign-in URL without opening a browser")
	return cmd
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

// resolveUIBaseURL derives the dashboard base URL. An explicit --ui-url wins;
// otherwise it takes the context server host and the default HTTP port.
func resolveUIBaseURL(override string) (string, error) {
	if override != "" {
		return strings.TrimRight(override, "/"), nil
	}
	server := viper.GetString("contexts.default.server")
	if server == "" {
		return fmt.Sprintf("http://localhost:%d", config.DefaultHTTPPort), nil
	}
	host := server
	if u, err := url.Parse(server); err == nil && u.Host != "" {
		host = u.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return fmt.Sprintf("http://%s:%d", host, config.DefaultHTTPPort), nil
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
