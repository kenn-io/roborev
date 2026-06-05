package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/agenthook"
)

func agentHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent-hook",
		Short: "Install and run optional agent harness hooks for roborev",
	}
	cmd.AddCommand(
		agentHookRunCmd(),
		agentHookInstallCmd(),
		agentHookDumpCmd(),
		agentHookDaemonCmd(),
		agentHookStatusCmd(),
		agentHookResetCmd(),
	)
	return cmd
}

func agentHookRunCmd() *cobra.Command {
	opts := agenthook.DefaultOptions()
	cmd := &cobra.Command{
		Use:                   "run",
		Short:                 "Read an agent hook payload from stdin and emit hook JSON",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := agenthook.ResolveOptions(opts, agentHookFlagChanges(cmd))
			if err != nil {
				return err
			}
			return runAgentHook(resolved, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	addAgentHookRunFlags(cmd, &opts)
	return cmd
}

func agentHookDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the local agent hook state daemon",
	}
	cmd.AddCommand(
		agentHookDaemonRunCmd(),
		&cobra.Command{
			Use:                   "start",
			Short:                 "Start the local agent hook state daemon",
			Args:                  cobra.NoArgs,
			DisableFlagsInUseLine: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return agenthook.RunDaemonStart(cmd.OutOrStdout())
			},
		},
		&cobra.Command{
			Use:                   "status",
			Short:                 "Print agent hook daemon process status as JSON",
			Args:                  cobra.NoArgs,
			DisableFlagsInUseLine: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return agenthook.RunDaemonStatus(cmd.OutOrStdout())
			},
		},
		&cobra.Command{
			Use:                   "stop",
			Short:                 "Stop the local agent hook state daemon",
			Args:                  cobra.NoArgs,
			DisableFlagsInUseLine: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return agenthook.RunDaemonStop(cmd.OutOrStdout())
			},
		},
		&cobra.Command{
			Use:                   "restart",
			Short:                 "Restart the local agent hook state daemon",
			Args:                  cobra.NoArgs,
			DisableFlagsInUseLine: true,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return agenthook.RunDaemonRestart(cmd.OutOrStdout())
			},
		},
	)
	return cmd
}

func agentHookDaemonRunCmd() *cobra.Command {
	addr := defaultAgentHookDaemonAddress()
	cmd := &cobra.Command{
		Use:                   "run",
		Short:                 "Run the local agent hook state daemon in the foreground",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		Hidden:                true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentHookDaemon(addr, cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&addr, "addr", addr, "daemon listen address")
	return cmd
}

func agentHookInstallCmd() *cobra.Command {
	opts := agenthook.InstallOptions{
		Agent:            "all",
		CodexConfigPath:  agenthook.DefaultCodexHooksPath(),
		ClaudeConfigPath: agenthook.DefaultClaudeSettingsPath(),
		Timeout:          10 * time.Second,
	}
	cmd := &cobra.Command{
		Use:                   "install",
		Short:                 "Install Codex and Claude agent hook entries",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			command, notice, err := agenthook.ResolveHookCommand(opts.Command)
			if err != nil {
				return err
			}
			if notice != "" {
				fmt.Fprintln(cmd.OutOrStdout(), notice)
			}
			opts.Command = command
			return agenthook.RunInstall(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.Agent, "agent", opts.Agent, "agent config to update: codex, claude, or all")
	cmd.Flags().StringVar(&opts.Command, "command", opts.Command, "hook command to install; defaults to this binary plus 'agent-hook run'")
	cmd.Flags().StringVar(&opts.CodexConfigPath, "codex-config", opts.CodexConfigPath, "Codex hooks.json path")
	cmd.Flags().StringVar(&opts.ClaudeConfigPath, "claude-config", opts.ClaudeConfigPath, "Claude settings.json path")
	cmd.Flags().Var(&agentHookSecondsOrDuration{d: &opts.Timeout}, "timeout", "Codex hook timeout (e.g. 10s, 1m, or bare integer seconds)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", opts.DryRun, "print what would change without writing files")
	return cmd
}

func agentHookDumpCmd() *cobra.Command {
	opts := agenthook.DumpOptions{Timeout: 10 * time.Second}
	cmd := &cobra.Command{
		Use:                   "dump",
		Short:                 "Print an agent's hook config as JSON",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			command, notice, err := agenthook.ResolveHookCommand(opts.Command)
			if err != nil {
				return err
			}
			// Notices are advisory warnings; keep them off stdout so the dumped
			// JSON config stays clean for piping.
			if notice != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), notice)
			}
			opts.Command = command
			return agenthook.RunDump(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.Agent, "agent", opts.Agent, "agent config to dump: codex or claude")
	cmd.Flags().StringVar(&opts.Command, "command", opts.Command, "hook command to install; defaults to this binary plus 'agent-hook run'")
	cmd.Flags().StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "config path to read and merge into; defaults to the agent's standard path")
	cmd.Flags().Var(&agentHookSecondsOrDuration{d: &opts.Timeout}, "timeout", "Codex hook timeout (e.g. 10s, 1m, or bare integer seconds)")
	return cmd
}

func agentHookStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:                   "status",
		Short:                 "Print tracked agent hook session counts as JSON",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return agenthook.RunStatus(cmd.OutOrStdout())
		},
	}
}

func agentHookResetCmd() *cobra.Command {
	opts := agenthook.ResetOptions{}
	cmd := &cobra.Command{
		Use:                   "reset [session-id]",
		Short:                 "Reset one agent hook session count, or all counts with --all",
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

func runAgentHook(opts agenthook.Options, stdin io.Reader, stdout, stderr io.Writer) error {
	var input agenthook.Input
	if err := json.NewDecoder(stdin).Decode(&input); err != nil {
		return fmt.Errorf("decode agent hook input: %w", err)
	}
	if input.SessionID == "" {
		return fmt.Errorf("agent hook input missing session_id")
	}

	resp, err := postAgentHook(context.Background(), agenthook.Request{
		Event:                 input,
		Threshold:             opts.TurnThreshold,
		CommitThreshold:       opts.CommitThreshold,
		FailedReviewThreshold: opts.FailedReviewThreshold,
		Instruction:           opts.Instruction,
		RoborevServerAddr:     opts.RoborevServerAddr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "roborev agent-hook: %v\n", err)
		return json.NewEncoder(stdout).Encode(map[string]any{})
	}

	return json.NewEncoder(stdout).Encode(agenthook.BuildOutput(input, resp))
}

func addAgentHookRunFlags(cmd *cobra.Command, opts *agenthook.Options) {
	cmd.Flags().StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "roborev config path")
	cmd.Flags().IntVar(&opts.TurnThreshold, "turn-threshold", opts.TurnThreshold, "Stop hook threshold; 0 disables Stop triggering")
	cmd.Flags().IntVar(&opts.CommitThreshold, "commit-threshold", opts.CommitThreshold, "PostToolUse commit threshold; 0 disables commit triggering")
	cmd.Flags().IntVar(&opts.FailedReviewThreshold, "failed-review-threshold", opts.FailedReviewThreshold, "open failed roborev review threshold; 0 disables review triggering")
	cmd.Flags().StringVar(&opts.Instruction, "instruction", opts.Instruction, "continuation instruction")
	cmd.Flags().StringVar(&opts.RoborevServerAddr, "roborev-server", opts.RoborevServerAddr, "roborev daemon address; defaults to runtime discovery")
}

func agentHookFlagChanges(cmd *cobra.Command) map[string]bool {
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

type agentHookSecondsOrDuration struct {
	d *time.Duration
}

func (s *agentHookSecondsOrDuration) String() string {
	if s.d == nil {
		return time.Duration(0).String()
	}
	return s.d.String()
}

func (s *agentHookSecondsOrDuration) Set(v string) error {
	if n, err := strconv.Atoi(v); err == nil {
		*s.d = time.Duration(n) * time.Second
		return nil
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		return err
	}
	if parsed%time.Second != 0 {
		return fmt.Errorf("timeout must be a whole number of seconds, got %s", v)
	}
	*s.d = parsed
	return nil
}

func (s *agentHookSecondsOrDuration) Type() string {
	return "duration"
}
