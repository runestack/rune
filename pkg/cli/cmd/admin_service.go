package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/api/generated"
	"github.com/spf13/cobra"
)

// permissionShorthands maps the user-facing --permissions vocabulary onto
// the built-in policies seeded at server bootstrap. Keep this list small
// and meaningful; anything more bespoke should be expressed as a real
// custom policy via `rune admin policy create`.
var permissionShorthands = map[string]string{
	"cast":  "cast",     // CI deploy: write services + read instances/logs
	"read":  "readonly", // get/list/watch on everything
	"admin": "admin",    // full access
}

func newAdminServiceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "service",
		Short: "Manage service accounts (non-human identities for CI/automation)",
		Long: `Manage service accounts — non-human identities used by CI pipelines,
deploy bots, and other automation. Service accounts authenticate exactly
like users (bearer token), but their tokens carry subject-type=service so
they are easy to audit and revoke separately from human users.`,
	}
	c.AddCommand(newAdminServiceCreateCmd())
	return c
}

func newAdminServiceCreateCmd() *cobra.Command {
	var namespace string
	var permissions []string
	var ttl time.Duration
	var description string
	var outFile string

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a service account, attach permissions, and issue a token",
		Long: `Create a service account in one shot: provisions the subject, attaches
the requested permissions, and issues a bearer token. Designed for the
CI use case where you want a scoped, namespaced token rather than the
overprivileged root token from 'admin bootstrap'.

Permissions:
  cast    Deploy services (create/update/scale + read instances/logs).
          The minimum needed by 'rune cast'. Recommended for CI.
  read    Read-only access (get/list/watch on everything).
  admin   Full access. Equivalent to the root token. Use sparingly.

When --namespace is given, the granted permissions are pinned to that
namespace via a derived per-service policy. Without --namespace, the
service account gets the cluster-wide built-in policy unchanged.

Examples:
  # Scoped CI token for the 'stg' namespace, deploy permissions only.
  rune admin service create ci-stg --namespace stg --permissions cast

  # Read-only token for a dashboard.
  rune admin service create dashboard --permissions read --ttl 720h

  # Pipe-friendly output for CI:
  rune admin service create ci-prod -n prod -p cast --out-file token.txt`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if len(permissions) == 0 {
				permissions = []string{"cast"}
			}
			// Resolve shorthands and dedupe.
			resolved := make([]string, 0, len(permissions))
			seen := make(map[string]bool)
			for _, raw := range permissions {
				for _, p := range strings.Split(raw, ",") {
					p = strings.TrimSpace(p)
					if p == "" {
						continue
					}
					base, ok := permissionShorthands[p]
					if !ok {
						return fmt.Errorf("unknown permission %q (valid: cast, read, admin)", p)
					}
					if seen[base] {
						continue
					}
					seen[base] = true
					resolved = append(resolved, base)
				}
			}

			api, err := newAPIClient("", "")
			if err != nil {
				return err
			}
			defer api.Close()

			adminCli := generated.NewAdminServiceClient(api.Conn())
			authCli := generated.NewAuthServiceClient(api.Conn())
			ctx := context.Background()

			// Determine which policies to attach. With --namespace, derive a
			// per-service policy with the namespace pinned so the service
			// account cannot reach into other namespaces. Without --namespace,
			// just attach the cluster-wide built-in by reference.
			policiesToAttach := make([]string, 0, len(resolved))
			for _, base := range resolved {
				if namespace == "" {
					policiesToAttach = append(policiesToAttach, base)
					continue
				}
				derived, err := deriveNamespacedPolicy(ctx, adminCli, base, name, namespace)
				if err != nil {
					return fmt.Errorf("derive %s policy for namespace %q: %w", base, namespace, err)
				}
				policiesToAttach = append(policiesToAttach, derived)
			}

			// Create the subject (idempotent: UserCreate upserts).
			if _, err := adminCli.UserCreate(ctx, &generated.UserCreateRequest{
				Name:     name,
				Policies: policiesToAttach,
			}); err != nil {
				return fmt.Errorf("create service account: %w", err)
			}

			// Issue the token. SubjectType=service is the whole point of
			// this command — it lets operators tell apart human and machine
			// tokens in `rune admin token list`.
			tokResp, err := authCli.CreateToken(ctx, &generated.CreateTokenRequest{
				Name:        name,
				SubjectName: name,
				SubjectType: "service",
				Description: description,
				Policies:    policiesToAttach,
				TtlSeconds:  int64(ttl / time.Second),
			})
			if err != nil {
				return fmt.Errorf("issue token: %w", err)
			}
			if tokResp.Secret == "" {
				return fmt.Errorf("server did not return a token secret")
			}

			if outFile != "" {
				if err := os.WriteFile(outFile, []byte(tokResp.Secret), 0o600); err != nil {
					return fmt.Errorf("write token to %s: %w", outFile, err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Service account %q created. Token written to %s (mode 0600).\n", name, outFile)
				return nil
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Service account %q created.\n", name)
			fmt.Fprintf(cmd.OutOrStdout(), "Permissions: %s\n", strings.Join(resolved, ", "))
			if namespace != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Namespace:   %s\n", namespace)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Token (save this — it will not be shown again):")
			fmt.Fprintln(cmd.OutOrStdout(), tokResp.Secret)
			return nil
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Pin granted permissions to this namespace (recommended for CI)")
	cmd.Flags().StringSliceVarP(&permissions, "permissions", "p", []string{"cast"}, "Permission set: cast | read | admin (comma-separated, repeatable)")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "Token time-to-live (e.g. 720h). 0 means no expiry.")
	cmd.Flags().StringVar(&description, "description", "", "Free-form description (shown in 'admin token list')")
	cmd.Flags().StringVar(&outFile, "out-file", "", "Write the token secret to this file (0600) instead of stdout")
	return cmd
}

// deriveNamespacedPolicy fetches a built-in policy and writes a copy with
// every rule's Namespace pinned to the given namespace, except for rules
// that operate on the cluster-scoped `namespaces` resource itself (those
// stay unpinned so the service account can still get/create the target
// namespace via --create-namespace). The derived policy is named
// `<service>-<base>` and is idempotent: calling create again upserts.
func deriveNamespacedPolicy(ctx context.Context, cli generated.AdminServiceClient, base, service, namespace string) (string, error) {
	resp, err := cli.PolicyGet(ctx, &generated.PolicyGetRequest{Name: base})
	if err != nil {
		return "", fmt.Errorf("get built-in policy %q: %w", base, err)
	}
	src := resp.GetPolicy()
	if src == nil {
		return "", fmt.Errorf("built-in policy %q not found", base)
	}
	derivedName := fmt.Sprintf("%s-%s", service, base)
	derived := &generated.Policy{
		Name:        derivedName,
		Description: fmt.Sprintf("%s permissions for service %q in namespace %q", base, service, namespace),
	}
	for _, r := range src.GetRules() {
		ns := namespace
		if r.GetResource() == "namespaces" {
			// Don't pin the rule that grants access to the namespaces
			// resource itself — pinning it would deny the lookup
			// during --create-namespace.
			ns = ""
		}
		derived.Rules = append(derived.Rules, &generated.PolicyRule{
			Resource:  r.GetResource(),
			Verbs:     r.GetVerbs(),
			Namespace: ns,
		})
	}
	if _, err := cli.PolicyCreate(ctx, &generated.PolicyCreateRequest{Policy: derived}); err != nil {
		return "", fmt.Errorf("create derived policy %q: %w", derivedName, err)
	}
	return derivedName, nil
}
