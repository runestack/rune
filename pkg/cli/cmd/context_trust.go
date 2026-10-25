package cmd

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// newContextTrustCmd adds `rune context trust`: it connects to a context's
// server, shows the SPKI fingerprint of the certificate the server presents,
// and — once confirmed — enables TLS on the context and pins that
// fingerprint. Trust on first use, so an operator reaching a self-signed,
// IP-addressed runed can pin without copying a CA file around.
func newContextTrustCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "trust [context-name]",
		Short: "Fetch, confirm and pin a context server's TLS certificate",
		Long: `Connect to the context's server, display the SPKI fingerprint of the
certificate it presents, and on confirmation enable TLS for the context and
pin that fingerprint (trust on first use).

Non-interactively, pass --yes to pin without the prompt, or set the
fingerprint yourself via the context's caFingerprint.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "default"
			if len(args) > 0 {
				name = args[0]
			}
			cfg, err := loadContextConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			ctx, ok := cfg.Contexts[name]
			if !ok {
				return fmt.Errorf("context %q not found", name)
			}
			if ctx.Server == "" {
				return fmt.Errorf("context %q has no server", name)
			}

			fp, err := fetchServerFingerprint(ensureDefaultGRPCPort(ctx.Server))
			if err != nil {
				return err
			}
			fmt.Printf("Server:      %s\nFingerprint: %s\n", ctx.Server, fp)

			if !yes {
				fmt.Print("Trust this certificate and pin it? [y/N]: ")
				reader := bufio.NewReader(os.Stdin)
				line, _ := reader.ReadString('\n')
				if s := strings.ToLower(strings.TrimSpace(line)); s != "y" && s != "yes" {
					fmt.Println("Not trusted; context unchanged.")
					return nil
				}
			}

			ctx.TLS = true
			ctx.CAFingerprint = fp
			cfg.Contexts[name] = ctx
			if err := saveContextConfig(cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Printf("Context %q now uses TLS, pinned to %s\n", name, fp)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Pin without the interactive prompt (trust on first use)")
	return cmd
}

// fetchServerFingerprint completes a TLS handshake with addr (verification
// off — we are establishing trust, not relying on it) and returns the SPKI
// fingerprint of the leaf certificate, as "sha256:<hex>".
func fetchServerFingerprint(addr string) (string, error) {
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // TOFU: we show the fingerprint for the operator to confirm
	if err != nil {
		return "", fmt.Errorf("tls handshake with %s: %w", addr, err)
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", fmt.Errorf("%s presented no certificate", addr)
	}
	sum := sha256.Sum256(certs[0].RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
