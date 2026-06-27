package main

import (
	"fmt"
	"time"

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
	cmd.AddCommand(droidHookInstallCmd())
	cmd.AddCommand(droidHookDumpCmd())
	cmd.AddCommand(droidHookStatusCmd())
	cmd.AddCommand(droidHookResetCmd())
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

func droidHookInstallCmd() *cobra.Command {
	hookBinary := ""
	opts := droidhook.InstallOptions{
		Scope:   "user",
		Timeout: 10 * time.Second,
	}
	cmd := &cobra.Command{
		Use:                   "install",
		Short:                 "Install Factory Droid hook entries",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			command, notice, err := agenthook.ResolveHookCommandWithRunner(opts.Command, hookBinary, droidhook.DroidRunner)
			if err != nil {
				return err
			}
			if notice != "" {
				fmt.Fprintln(cmd.OutOrStdout(), notice)
			}
			opts.Command = command
			return droidhook.RunInstall(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.Command, "command", opts.Command, "hook command to install; defaults to this binary plus 'droid-hook run'")
	cmd.Flags().StringVar(&hookBinary, "binary", "", "roborev binary path to bake into Droid hooks (for version-manager shims)")
	cmd.Flags().StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "Factory Droid hooks.json path; defaults to the resolved scope's standard path")
	cmd.Flags().StringVar(&opts.Scope, "scope", opts.Scope, "config scope to update: user or project")
	cmd.Flags().Var(&agentHookSecondsOrDuration{d: &opts.Timeout}, "timeout", "Droid hook timeout (e.g. 10s, 1m, or bare integer seconds)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", opts.DryRun, "print what would change without writing files")
	return cmd
}

func droidHookDumpCmd() *cobra.Command {
	opts := droidhook.DumpOptions{
		Scope:   "user",
		Timeout: 10 * time.Second,
	}
	cmd := &cobra.Command{
		Use:                   "dump",
		Short:                 "Print the Factory Droid hook config as JSON",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			command, notice, err := agenthook.ResolveHookCommandWithRunner(opts.Command, "", droidhook.DroidRunner)
			if err != nil {
				return err
			}
			// Notices are advisory warnings; keep them off stdout so the dumped
			// JSON config stays clean for piping.
			if notice != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), agenthook.TranslateBinaryNotice(notice))
			}
			opts.Command = command
			return droidhook.RunDump(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.Command, "command", opts.Command, "hook command to install; defaults to this binary plus 'droid-hook run'")
	cmd.Flags().StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "Factory Droid hooks.json path to read and merge into; defaults to the resolved scope's standard path")
	cmd.Flags().StringVar(&opts.Scope, "scope", opts.Scope, "config scope to dump: user or project")
	cmd.Flags().Var(&agentHookSecondsOrDuration{d: &opts.Timeout}, "timeout", "Droid hook timeout (e.g. 10s, 1m, or bare integer seconds)")
	return cmd
}

// droidHookStatusCmd and droidHookResetCmd delegate to the shared agenthook
// daemon that droid-hook run feeds. The session state is integration-agnostic
// (keyed by session_id), so Droid sessions are inspected and reset through the
// same surface as Codex/Claude sessions.
func droidHookStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:                   "status",
		Short:                 "Print tracked Factory Droid hook session counts as JSON",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return agenthook.RunStatus(cmd.OutOrStdout())
		},
	}
}

func droidHookResetCmd() *cobra.Command {
	opts := agenthook.ResetOptions{}
	cmd := &cobra.Command{
		Use:                   "reset [session-id]",
		Short:                 "Reset one Factory Droid hook session count, or all counts with --all",
		Args:                  cobra.MaximumNArgs(1),
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := ""
			if len(args) > 0 {
				sessionID = args[0]
			}
			return agenthook.RunReset(opts, sessionID, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&opts.All, "all", false, "reset all sessions")
	return cmd
}
