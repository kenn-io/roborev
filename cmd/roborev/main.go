package main

import (
	"errors"
	"os"

	"github.com/spf13/cobra"
)

var (
	serverAddr string
	verbose    bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "roborev",
		Short: "Automatic code review for git commits",
		Long:  "roborev automatically reviews git commits using AI agents (Codex, Claude Code, Gemini, Copilot, OpenCode, Cursor, Kiro, Pi)",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// validateServerFlag is itself part of the invocation contract
			// (an invalid --server value is a usage problem, not a runtime
			// problem), so run it first while SilenceUsage is still false.
			if err := validateServerFlag(); err != nil {
				return err
			}
			// Past this point cobra has validated everything it can, so
			// errors from RunE just get a plain "Error: ..." line without
			// the usage wall.
			cmd.SilenceUsage = true
			return nil
		},
	}

	rootCmd.PersistentFlags().StringVar(&serverAddr, "server", "", "daemon server address (e.g. 127.0.0.1:7373 or unix://)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")

	rootCmd.AddCommand(initCmd())
	rootCmd.AddCommand(quickstartCmd())
	rootCmd.AddCommand(reviewCmd())
	rootCmd.AddCommand(postCommitCmd())
	rootCmd.AddCommand(enqueueCmd()) // hidden alias for backward compatibility
	rootCmd.AddCommand(waitCmd())
	rootCmd.AddCommand(statusCmd())
	rootCmd.AddCommand(pauseCmd())
	rootCmd.AddCommand(unpauseCmd())
	rootCmd.AddCommand(listCmd())
	rootCmd.AddCommand(showCmd())
	rootCmd.AddCommand(commentCmd())
	rootCmd.AddCommand(respondCmd()) // hidden alias for backward compatibility
	rootCmd.AddCommand(closeCmd())
	rootCmd.AddCommand(installHookCmd())
	rootCmd.AddCommand(uninstallHookCmd())
	rootCmd.AddCommand(daemonCmd())
	rootCmd.AddCommand(streamCmd())
	rootCmd.AddCommand(tuiCmd())
	rootCmd.AddCommand(refineCmd())
	rootCmd.AddCommand(runCmd())
	rootCmd.AddCommand(analyzeCmd())
	rootCmd.AddCommand(insightsCmd())
	rootCmd.AddCommand(fixCmd())
	rootCmd.AddCommand(compactCmd())
	rootCmd.AddCommand(promptCmd()) // hidden alias for backward compatibility
	rootCmd.AddCommand(exportCmd())
	rootCmd.AddCommand(repoCmd())
	rootCmd.AddCommand(skillsCmd())
	rootCmd.AddCommand(syncCmd())
	rootCmd.AddCommand(remapCmd())
	rootCmd.AddCommand(agentHookCmd())
	rootCmd.AddCommand(checkAgentsCmd())
	rootCmd.AddCommand(ciCmd())
	rootCmd.AddCommand(logCmd())
	rootCmd.AddCommand(summaryCmd())
	rootCmd.AddCommand(costCmd())
	rootCmd.AddCommand(backfillVerdictsCmd())
	rootCmd.AddCommand(configCmd())
	rootCmd.AddCommand(backfillTokensCmd())
	rootCmd.AddCommand(updateCmd())
	rootCmd.AddCommand(versionCmd())

	if err := rootCmd.Execute(); err != nil {
		// exitError carries a specific exit code; the RunE that returned
		// it has already silenced cobra's error printing via silentExit.
		var exitErr *exitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.code)
		}
		// All other errors: cobra already printed them.
		os.Exit(1)
	}
}
