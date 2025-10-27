package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/runestack/rune/pkg/cli/format"
	"github.com/runestack/rune/pkg/cli/utils"
	"github.com/runestack/rune/pkg/types"
	"github.com/spf13/cobra"
)

var (
	lintStrict        bool
	lintQuiet         bool
	lintRecursive     bool
	lintExitOnFail    bool
	lintAutoFix       bool
	lintOutputFormat  string
	lintContextLines  int
	lintExpandContext bool
)

// lintCmd represents the lint command
var lintCmd = &cobra.Command{
	Use:   "lint [file or directory]...",
	Short: "Validate YAML specifications and Rune configuration files",
	Long: `Lint Rune specs. Determines whether a file is a Rune config (runefile)
or a resource spec (castfile). Runs the appropriate validator and reports all
issues with helpful context. Examples:

  rune lint myservice.yaml
  rune lint ./manifests --recursive
  rune lint examples/config/rune.yaml --format json`,
	Args: cobra.MinimumNArgs(1),
	RunE: runLintCmd,
}

func init() {
	rootCmd.AddCommand(lintCmd)

	lintCmd.Flags().BoolVar(&lintStrict, "strict", false, "Enable stricter validation rules (reserved)")
	lintCmd.Flags().BoolVar(&lintQuiet, "quiet", false, "Only show errors, no progress or success messages")
	lintCmd.Flags().BoolVarP(&lintRecursive, "recursive", "r", false, "Recursively process directories")
	lintCmd.Flags().BoolVar(&lintExitOnFail, "exit-on-fail", false, "Exit on first validation failure")
	lintCmd.Flags().BoolVar(&lintAutoFix, "fix", false, "Attempt to auto-fix simple issues when possible")
	lintCmd.Flags().StringVar(&lintOutputFormat, "format", "text", "Output format (text|json)")
	lintCmd.Flags().IntVar(&lintContextLines, "context", 1, "Number of context lines to show around errors")
	lintCmd.Flags().BoolVar(&lintExpandContext, "expand-context", false, "Show more context around errors (equivalent to --context=3)")
}

func runLintCmd(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("at least one file or directory is required")
	}

	if lintExpandContext {
		lintContextLines = 3
	}

	// Expand inputs into a list of YAML file paths
	filesToLint, err := utils.ExpandFilePaths(args, lintRecursive)
	if err != nil {
		return err
	}
	if len(filesToLint) == 0 {
		return fmt.Errorf("no YAML files found to lint")
	}

	startTime := time.Now()
	totalErrorCount := 0
	totalFixCount := 0
	hasErrors := false

	// For JSON mode, aggregate all errors across files
	aggregated := struct {
		Files       []string                 `json:"files"`
		Errors      []format.ValidationError `json:"errors"`
		ErrorCount  int                      `json:"error_count"`
		FixCount    int                      `json:"fix_count"`
		Success     bool                     `json:"success"`
		TimeSeconds float64                  `json:"time"`
	}{Files: make([]string, 0), Errors: make([]format.ValidationError, 0)}

	for _, filePath := range filesToLint {
		// Read file contents for context-aware formatting
		data, readErr := os.ReadFile(filePath)
		if readErr != nil {
			if lintOutputFormat == "text" {
				fmt.Printf("%s %s\n", format.StatusSymbol(false), filePath)
			}
			return fmt.Errorf("error reading %s: %w", filePath, readErr)
		}

		if lintOutputFormat == "text" && !lintQuiet {
			printFileLabel(filePath)
		}

		// Lint the file (handles detection, printing detailed errors, and autofix)
		formatter, fileFixes := lintFile(filePath, data)
		totalFixCount += fileFixes

		// Finalize per-file reporting and aggregation
		if len(formatter.Errors) == 0 {
			if lintOutputFormat == "text" && !lintQuiet {
				fmt.Println("✓")
			}
		} else {
			hasErrors = true
			totalErrorCount += len(formatter.Errors)
			if lintOutputFormat == "text" {
				fmt.Println("❌")
				formatter.PrintErrorSummary()
			}
		}

		if lintOutputFormat == "json" {
			aggregated.Files = append(aggregated.Files, filePath)
			aggregated.Errors = append(aggregated.Errors, formatter.Errors...)
		}

		if lintExitOnFail && len(formatter.Errors) > 0 {
			break
		}
	}

	if lintOutputFormat == "json" {
		aggregated.ErrorCount = totalErrorCount
		aggregated.FixCount = totalFixCount
		aggregated.Success = totalErrorCount == 0
		aggregated.TimeSeconds = time.Since(startTime).Seconds()
		b, _ := json.MarshalIndent(aggregated, "", "  ")
		fmt.Println(string(b))
	} else {
		// Print overall stats in text mode
		format.PrintLintSummary(len(filesToLint), totalErrorCount, totalFixCount, time.Since(startTime))
	}

	if hasErrors || totalErrorCount > 0 {
		return fmt.Errorf("")
	}
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// printFileLabel prints a dotted label for a file path
func printFileLabel(filePath string) {
	base := filepath.Base(filePath)
	label := fmt.Sprintf("- %-20s", base)
	dots := strings.Repeat(".", max(3, 35-len(label)))
	fmt.Printf("%s%s ", label, dots)
}

// lintFile detects file type, runs linting, prints detailed errors using the formatter,
// attempts autofix if enabled, and returns the formatter and number of fixes applied.
func lintFile(filePath string, data []byte) (*format.ErrorFormatter, int) {
	formatter := format.NewErrorFormatter(filePath, data)
	formatter.ContextLines = lintContextLines
	formatter.CanAutoFix = lintAutoFix
	formatter.OutputFormat = lintOutputFormat

	// Initial detection and linting
	runDetectionAndLint := func() {
		// Check if the file is a rune config file
		isRuneConfig, detectErr := types.IsRuneConfigFile(filePath)
		if detectErr != nil {
			formatter.PrintError(fmt.Sprintf("failed to inspect file: %v", detectErr), formatter.ExtractLineNumber(detectErr.Error()))
			return
		}
		if isRuneConfig {
			if rf, err := types.ParseRuneFile(filePath); err != nil {
				formatter.PrintError(err.Error(), formatter.ExtractLineNumber(err.Error()))
			} else {
				if errs := rf.Lint(); len(errs) > 0 {
					for _, e := range errs {
						ln := formatter.ExtractLineNumber(e.Error())
						formatter.PrintError(e.Error(), ln)
					}
				}
			}
			return
		}

		// Try parsing as cast file directly (AST-based, tolerant of duplicate keys)
		if cf, err := types.ParseCastFile(filePath, ""); err == nil {
			if errs := cf.Lint(); len(errs) > 0 {
				for _, e := range errs {
					ln := formatter.ExtractLineNumber(e.Error())
					formatter.PrintError(e.Error(), ln)
				}
			}
			return
		}
		formatter.PrintError("unrecognized resource type: file does not appear to be a rune config or a resource cast file", 0)
	}

	runDetectionAndLint()

	// Attempt autofix and re-lint once
	totalFixes := 0
	if lintAutoFix && len(formatter.Errors) > 0 {
		anyFixed := false
		for _, valErr := range formatter.Errors {
			if fixed, newData := formatter.TryAutoFix(valErr.Message, valErr.LineNumber); fixed {
				if wErr := os.WriteFile(filePath, newData, 0o600); wErr == nil {
					anyFixed = true
					totalFixes++
					formatter.FixCount++
					//data = newData
					formatter.FileData = newData
				}
			}
		}
		if anyFixed {
			formatter.Errors = nil
			runDetectionAndLint()
		}
	}

	return formatter, totalFixes
}

// hasYAMLExtension checks if a file has a YAML extension
func hasYAMLExtension(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	return ext == ".yaml" || ext == ".yml"
}
