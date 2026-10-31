package main

import (
	"os"
	// Embed the IANA timezone database as a last-resort fallback for
	// time.LoadLocation: releases are static CGO_ENABLED=0 binaries, and
	// Windows and minimal container images have no system zoneinfo, which
	// would make named timezones (e.g. [ci.quiet_hours] timezone) fail.
	_ "time/tzdata"

	"github.com/spf13/cobra"
)

var (
	serverAddr string
	verbose    bool
	fromSkill  bool
)

func main() {
	code := executeCLI(newRootCmd(), os.Args[1:])
	waitCLIUse()
	if code != 0 {
		os.Exit(code)
	}
}

func newRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "roborev",
		Short:         "Automatic code review for git commits",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long:          "roborev automatically reviews git commits using AI agents (Codex, Claude Code, Gemini, Copilot, OpenCode, Cursor, Kiro, Pi)",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if err := validateServerFlag(); err != nil {
				if isCLIHook(cmd) {
					return err
				}
				return validationErr(cmd, err)
			}
			if err := validateRemoteCommandFor(cmd); err != nil {
				if isCLIHook(cmd) {
					return err
				}
				return usageErr(cmd, err)
			}
			// Runtime failures are rendered centrally without a usage block.
			cmd.SilenceUsage = true
			armCLIUse(cmd)
			return nil
		},
	}

	rootCmd.PersistentFlags().StringVar(&serverAddr, "server", "", "daemon address (loopback, unix://, or explicit remote HTTPS base URL)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output")
	rootCmd.PersistentFlags().BoolVar(&fromSkill, "from-skill", false, "set by bundled agent skills; the command is not counted as an app open")
	_ = rootCmd.PersistentFlags().MarkHidden("from-skill")

	rootCmd.AddCommand(signingInitCmd())
	rootCmd.AddCommand(initCmd())
	rootCmd.AddCommand(quickstartCmd())
	rootCmd.AddCommand(reviewCmd())
	rootCmd.AddCommand(postCommitCmd())
	rootCmd.AddCommand(enqueueCmd()) // hidden alias for backward compatibility
	rootCmd.AddCommand(waitCmd())
	rootCmd.AddCommand(statusCmd())
	rootCmd.AddCommand(snoozeCmd())
	rootCmd.AddCommand(pauseCmd())
	rootCmd.AddCommand(unpauseCmd())
	rootCmd.AddCommand(listCmd())
	rootCmd.AddCommand(showCmd())
	rootCmd.AddCommand(searchCmd())
	rootCmd.AddCommand(commentCmd())
	rootCmd.AddCommand(respondCmd()) // hidden alias for backward compatibility
	rootCmd.AddCommand(closeCmd())
	rootCmd.AddCommand(cancelCmd())
	rootCmd.AddCommand(installHookCmd())
	rootCmd.AddCommand(uninstallHookCmd())
	rootCmd.AddCommand(daemonCmd())
	rootCmd.AddCommand(streamCmd())
	rootCmd.AddCommand(tuiCmd())
	rootCmd.AddCommand(uiCmd())
	rootCmd.AddCommand(mcpCmd())
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
	rootCmd.AddCommand(doctorCmd())
	rootCmd.AddCommand(ciCmd())
	rootCmd.AddCommand(logCmd())
	rootCmd.AddCommand(summaryCmd())
	rootCmd.AddCommand(costCmd())
	rootCmd.AddCommand(backfillVerdictsCmd())
	rootCmd.AddCommand(legacyReviewsCmd())
	rootCmd.AddCommand(configCmd())
	rootCmd.AddCommand(backfillTokensCmd())
	rootCmd.AddCommand(updateCmd())
	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(verifyWebAssetsCmd())

	applyCommandGrammar(rootCmd)
	applyRootHelp(rootCmd)
	configureCLIErrors(rootCmd)
	return rootCmd
}
