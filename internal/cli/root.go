package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const version = "0.6.1"

// Exit codes per spec section 6.5
const (
	ExitSuccess      = 0
	ExitFindings     = 1
	ExitUsageError   = 2
	ExitAuthError    = 3
	ExitRuntimeError = 4
)

var rootCmd = &cobra.Command{
	Use:   "prism",
	Short: "Local AI code review CLI",
	Long:  "Prism reviews code changes using LLM providers and emits findings with deterministic exit codes.",
}

// Run executes the root command and returns an exit code.
func Run() int {
	rootCmd.AddCommand(reviewCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(modelsCmd)
	rootCmd.AddCommand(cacheCmd)
	rootCmd.AddCommand(hookCmd)
	rootCmd.AddCommand(githubCmd)
	rootCmd.AddCommand(versionCmd)

	if err := rootCmd.Execute(); err != nil {
		// Cobra already prints the error
		return ExitUsageError
	}

	return exitCode
}

// exitCode is set by command handlers to control the process exit code.
var exitCode = ExitSuccess

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print prism version",
	Run: func(cmd *cobra.Command, args []string) {
		_, _ = fmt.Fprintf(os.Stdout, "prism version %s\n", version)
	},
}
