package cmd

import (
	"fmt"

	"github.com/runestack/rune/pkg/version"
	"github.com/spf13/cobra"
)

// versionCmd represents the version command
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show the Rune version information",
	Long:  `Display detailed version information about the Rune binary.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(version.Info())
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
