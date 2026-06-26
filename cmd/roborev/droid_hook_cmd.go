package main

import (
	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/droidhook"
)

func droidHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "droid-hook",
		Short: "Install and run optional Factory Droid harness hooks for roborev",
		Long: `Install and run optional Factory Droid harness hooks for roborev.

The Stop hook blocks Factory Droid from finishing a session while roborev has
unresolved failed review findings, mirroring the roborev agent-hook integration
for Codex and Claude Code. It delegates to the same shared agenthook state
daemon as agent-hook, so loop prevention, PostToolUse commit tracking, and
failed-review detection are identical to Codex/Claude behavior.`,
	}
	cmd.AddCommand(droidHookRunCmd())
	return cmd
}

func droidHookRunCmd() *cobra.Command {
	opts := droidhook.DefaultOptions()
	cmd := &cobra.Command{
		Use:                   "run",
		Short:                 "Read a Factory Droid hook payload from stdin and emit hook JSON",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := droidhook.ResolveOptions(opts, droidHookFlagChanges(cmd))
			if err != nil {
				return err
			}
			return runHook(resolved, "droid-hook", cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	addDroidHookRunFlags(cmd, &opts)
	return cmd
}

func addDroidHookRunFlags(cmd *cobra.Command, opts *agenthook.Options) {
	cmd.Flags().StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "roborev config path")
	cmd.Flags().IntVar(&opts.TurnThreshold, "turn-threshold", opts.TurnThreshold, "Stop hook threshold; 0 disables Stop triggering")
	cmd.Flags().IntVar(&opts.CommitThreshold, "commit-threshold", opts.CommitThreshold, "PostToolUse commit threshold; 0 disables commit triggering")
	cmd.Flags().IntVar(&opts.FailedReviewThreshold, "failed-review-threshold", opts.FailedReviewThreshold, "open failed roborev review threshold; 0 disables review triggering")
	cmd.Flags().StringVar(&opts.Instruction, "instruction", opts.Instruction, "continuation instruction")
	cmd.Flags().StringVar(&opts.RoborevServerAddr, "roborev-server", opts.RoborevServerAddr, "roborev daemon address; defaults to runtime discovery")
}

func droidHookFlagChanges(cmd *cobra.Command) map[string]bool {
	flags := cmd.Flags()
	names := []string{
		"config",
		"turn-threshold",
		"commit-threshold",
		"failed-review-threshold",
		"instruction",
		"roborev-server",
	}
	changed := make(map[string]bool, len(names))
	for _, name := range names {
		changed[name] = flags.Changed(name)
	}
	return changed
}
