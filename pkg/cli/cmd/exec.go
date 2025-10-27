package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/runestack/rune/pkg/api/client"
	"github.com/runestack/rune/pkg/types"
	"github.com/runestack/rune/pkg/utils"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// parsedExecOptions defines the configuration for the exec command
type parsedExecOptions struct {
	workdir string
	env     map[string]string
	tty     bool
	timeout time.Duration
}

type execOptions struct {
	cmdOptions

	workdir string
	env     []string
	tty     bool
	noTTY   bool
	timeout string
}

// execCmd represents the exec command

func newExecCmd() *cobra.Command {
	opts := &execOptions{}
	cmd := &cobra.Command{
		Use:   "exec TARGET COMMAND [args...]",
		Short: "Execute a command in a running service or instance",
		Long: `Execute commands directly within running service instances.
	
	This command provides interactive access to containers and processes,
	allowing for real-time debugging, configuration verification, and
	emergency maintenance tasks.
	
	Examples:
	  # Start an interactive bash session in a service
	  rune exec api bash
	
	  # Execute a one-off command
	  rune exec api ls -la /app
	
	  # Execute in a specific instance
	  rune exec api-instance-123 ps aux
	
	  # Set working directory and environment variables
	  rune exec api --workdir=/app --env=DEBUG=true python debug.py
	
	  # Disable TTY for non-interactive commands
	  rune exec api --no-tty python script.py
	
	  # Set a timeout for the exec session
	  rune exec api --timeout=30s python long-running-script.py`,
		Aliases: []string{"e"},
		Args:    cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.namespace = effectiveCmdNS(opts.namespace)
			return runExec(cmd, args, opts)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Add flags
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "", "namespace of the service/instance")
	cmd.Flags().StringVar(&opts.workdir, "workdir", "", "working directory for the command")
	cmd.Flags().StringArrayVar(&opts.env, "env", []string{}, "environment variables to set (key=value format)")
	cmd.Flags().BoolVarP(&opts.tty, "tty", "t", false, "allocate a pseudo-TTY (defaults to true for interactive commands)")
	cmd.Flags().BoolVar(&opts.noTTY, "no-tty", false, "disable TTY allocation for non-interactive commands")
	cmd.Flags().StringVar(&opts.timeout, "timeout", "5m", "timeout for the exec session")
	cmd.Flags().StringVar(&opts.addressOverride, "api-server", "", "address of the API server")

	// After the first positional arg (TARGET), stop parsing flags so command args like '-c' are passed through
	cmd.Flags().SetInterspersed(false)

	return cmd
}

func init() { rootCmd.AddCommand(newExecCmd()) }

func runExec(cmd *cobra.Command, args []string, opts *execOptions) error {
	// Parse arguments
	target := args[0]
	command := args[1:]

	if len(command) == 0 {
		return fmt.Errorf("command cannot be empty")
	}

	// Parse parsedOpts
	parsedOpts, err := parseExecOptions(opts)
	if err != nil {
		return fmt.Errorf("failed to parse options: %w", err)
	}

	// Create API client
	apiClient, err := createAPIClient(&opts.cmdOptions)
	if err != nil {
		return fmt.Errorf("failed to connect to API server: %w", err)
	}
	defer apiClient.Close()

	// Resolve target
	resolvedTarget, err := resolveResourceTarget(apiClient, target, opts.namespace)
	if err != nil {
		return fmt.Errorf("failed to resolve target: %w", err)
	}

	// Create exec client
	execClient := client.NewExecClient(apiClient)

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), parsedOpts.timeout)
	defer cancel()

	// Create exec session
	session, err := execClient.NewExecSession(ctx)
	if err != nil {
		return fmt.Errorf("failed to create exec session: %w", err)
	}
	defer session.Close()

	// If neither --tty nor --no-tty explicitly set, auto-detect based on the command
	if !opts.noTTY && !opts.tty {
		parsedOpts.tty = shouldAllocateTTY(command)
	}

	// Initialize the session
	execOptions := &client.ExecOptions{
		Command:    command,
		Env:        parsedOpts.env,
		WorkingDir: parsedOpts.workdir,
		TTY:        parsedOpts.tty,
		Timeout:    parsedOpts.timeout,
	}

	// Set terminal size if TTY is enabled
	if parsedOpts.tty {
		if width, height, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			execOptions.TerminalWidth = utils.ToUint32NonNegative(width)
			execOptions.TerminalHeight = utils.ToUint32NonNegative(height)
		}
	}

	if err := initializeExecSession(session, resolvedTarget, execOptions); err != nil {
		return fmt.Errorf("failed to initialize exec session: %w", err)
	}

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	// Run the session
	var sessionErr error
	if parsedOpts.tty {
		sessionErr = session.RunInteractive()
	} else {
		sessionErr = session.RunNonInteractive()
	}

	// Check for context cancellation
	select {
	case <-ctx.Done():
		return fmt.Errorf("exec session timed out after %v", parsedOpts.timeout)
	default:
		return sessionErr
	}
}

func initializeExecSession(session *client.ExecSession, resolvedTarget *resolvedResourceTarget, execOptions *client.ExecOptions) error {
	if resolvedTarget.targetType == types.ResourceTypeInstance {
		return session.InitializeInstanceTarget(resolvedTarget.target, resolvedTarget.namespace, execOptions)
	} else {
		return session.InitializeServiceTarget(resolvedTarget.target, resolvedTarget.namespace, execOptions)
	}
}

func parseExecOptions(opts *execOptions) (*parsedExecOptions, error) {
	parsedOpts := &parsedExecOptions{
		workdir: opts.workdir,
		tty:     opts.tty,
	}

	// Parse timeout
	timeout, err := time.ParseDuration(opts.timeout)
	if err != nil {
		return nil, fmt.Errorf("invalid timeout format: %w", err)
	}
	parsedOpts.timeout = timeout

	// Parse environment variables
	parsedOpts.env = make(map[string]string)
	for _, env := range opts.env {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid environment variable format: %s (expected key=value)", env)
		}
		parsedOpts.env[parts[0]] = parts[1]
	}

	// Determine TTY allocation
	if opts.noTTY {
		parsedOpts.tty = false
	} else if !opts.tty {
		// Auto-detect TTY for interactive commands
		parsedOpts.tty = shouldAllocateTTY(os.Args[2:]) // Skip "rune" and "exec"
	}

	return parsedOpts, nil
}

// shouldAllocateTTY determines if TTY allocation is appropriate for the command.
func shouldAllocateTTY(command []string) bool {
	if len(command) == 0 {
		return false
	}

	// Common interactive shells
	interactiveShells := []string{"bash", "sh", "zsh", "fish", "tcsh", "dash"}
	for _, shell := range interactiveShells {
		if command[0] == shell {
			return true
		}
	}

	// Commands that benefit from TTY
	ttyCommands := []string{"vim", "nano", "top", "htop", "less", "more", "vi"}
	for _, cmd := range ttyCommands {
		if command[0] == cmd {
			return true
		}
	}

	// For other commands, default to non-TTY for better script compatibility
	return false
}
