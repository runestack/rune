package cmd

import (
	"fmt"
	"os"

	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/version"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc/status"
)

var (
	cfgFile  string
	verbose  bool
	logLevel string
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "rune",
	Short: "Rune - Lightweight Orchestration Platform",
	Long: `Rune is a lightweight, single-binary orchestration platform 
inspired by Kubernetes and Nomad. It is designed for developers 
who want to deploy and scale services quickly and intuitively, 
without unnecessary complexity.`,
	Run: func(cmd *cobra.Command, args []string) {
		// If no subcommand is specified, display the help
		if len(args) == 0 {
			cmd.Help()
			return
		}
	},
	Version: version.Version,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		if st, ok := status.FromError(err); ok {
			fmt.Printf("Error: %s\n", st.Message())
		} else {
			fmt.Println(err)
		}
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	// Here you will define your flags and configuration settings.
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.rune/config.yaml)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose output")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")

	// Add global environment variables
	viper.SetEnvPrefix("RUNE")
	viper.AutomaticEnv() // read in environment variables that match

	// Bind specific environment variables to config keys
	viper.BindEnv("log.level", "RUNE_LOG_LEVEL")

	// Register deps command group
	rootCmd.AddCommand(newDepsCmd())
	// Register auth-related commands
	rootCmd.AddCommand(newLoginCmd())
	rootCmd.AddCommand(newWhoAmICmd())
	// Admin command group (bootstrap, token, user, policy)
	rootCmd.AddCommand(newAdminCmd())
	// Register config management commands
	rootCmd.AddCommand(newConfigCmd())
	// Register convenient context switching alias
	rootCmd.AddCommand(newUseContextCmd())

	// Cleaner error UX: avoid usage dump and duplicate error printing
	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else if cfgEnv, ok := os.LookupEnv("RUNE_CLI_CONFIG"); ok && cfgEnv != "" {
		// Allow overriding config file path via environment variable
		viper.SetConfigFile(cfgEnv)
	} else {
		// Find home directory.
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		// Search config in home directory with name ".rune" (without extension).
		viper.AddConfigPath(home + "/.rune")
		viper.SetConfigName("config")
		viper.SetConfigType("yaml")
	}

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err == nil {
		if verbose {
			fmt.Println("Using config file:", viper.ConfigFileUsed())
		}
	}

	// Mirror current context into viper keys for global access
	if err := loadCurrentContextIntoViper(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// Configure log level
	configureLogLevel()
}

// configureLogLevel sets the global log level based on command line flags and environment variables
func configureLogLevel() {
	// Get log level from flag, environment variable, or config file
	levelStr := logLevel
	if levelStr == "" {
		levelStr = viper.GetString("log.level")
	}
	if levelStr == "" {
		levelStr = "info" // default
	}

	// Parse and set the log level
	level, err := log.ParseLevel(levelStr)
	if err != nil {
		fmt.Printf("Invalid log level: %s, defaulting to 'info'\n", levelStr)
		level = log.InfoLevel
	}

	// Create a new logger with the specified level and set it as the default
	logger := log.NewLogger(log.WithLevel(level))
	log.SetDefaultLogger(logger)
}

// getEnv is a small wrapper so other commands can mock env lookup in tests
func getEnv(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	return v, ok
}

// loadCurrentContextIntoViper reads our YAML contexts and mirrors the current context
// into viper under contexts.default.* so commands can rely on viper regardless of how
// the YAML is structured or which context is active.
func loadCurrentContextIntoViper() error {
	cfg, err := loadContextConfig()
	if err != nil || cfg == nil {
		return err
	}
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return nil
	}
	if ctx.Server != "" {
		viper.Set("contexts.default.server", ctx.Server)
	}
	if ctx.Token != "" {
		viper.Set("contexts.default.token", ctx.Token)
	}
	if ctx.DefaultNamespace != "" {
		viper.Set("contexts.default.defaultNamespace", ctx.DefaultNamespace)
	}
	return nil
}

// newUseContextCmd creates a convenient alias for 'rune config use-context'
func newUseContextCmd() *cobra.Command {
	// Reuse the existing config use-context command
	useContextCmd := newConfigUseContextCmd()

	// Update the command to be a top-level alias
	useContextCmd.Use = "use-context [context-name]"
	useContextCmd.Short = "Switch to a different context (alias for 'rune config use-context')"
	useContextCmd.Long = `Switch to a different context.

This is a convenient alias for 'rune config use-context'.
The context must already exist in your configuration.`

	return useContextCmd
}
