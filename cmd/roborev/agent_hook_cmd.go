package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	kitagenthook "go.kenn.io/kit/agenthook"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/githook"
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
	agent := ""
	source := ""
	cmd := &cobra.Command{
		Use:                   "run",
		Short:                 "Read an agent hook payload from stdin and emit hook JSON",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rawAgent := strings.ToLower(strings.TrimSpace(agent))
			if rawAgent == "" {
				// Releases before v0.64 installed profile-less Codex and Claude
				// commands. Keep that dispatcher through v0.66 so existing hooks
				// continue working until the bounded migration in #1012 replaces
				// them with profile-specific kit registrations.
				resolved, err := agenthook.ResolveOptionsForAgent("", opts, agentHookFlagChanges(cmd))
				if err != nil {
					return err
				}
				return runLegacyAgentHook(
					cmd.Context(), resolved, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				)
			}
			resolved, err := agenthook.ResolveOptionsForAgent(rawAgent, opts, agentHookFlagChanges(cmd))
			if err != nil {
				return err
			}
			if rawAgent == string(agenthook.AgentGrok) {
				return runGrokAgentHook(resolved, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			profile, err := kitagenthook.ParseAgent(rawAgent)
			if err != nil {
				return err
			}
			return runAgentHook(profile, resolved, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	addAgentHookRunFlags(cmd, &opts)
	cmd.Flags().StringVar(&agent, "agent", agent, "agent hook profile for this run")
	cmd.Flags().StringVar(&source, "source", source, "agent hook registration owner")
	_ = cmd.Flags().MarkHidden("source")
	return cmd
}

func runGrokAgentHook(opts agenthook.Options, stdin io.Reader, stdout, stderr io.Writer) error {
	input, err := agenthook.DecodeInput(stdin)
	if err != nil {
		return fmt.Errorf("decode Grok Build input: %w", err)
	}
	if input.SessionID == "" {
		return fmt.Errorf("decode Grok Build input: missing session_id")
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
		fmt.Fprintf(stderr, "roborev Grok Build: %v\n", err)
		return json.NewEncoder(stdout).Encode(map[string]any{})
	}
	return json.NewEncoder(stdout).Encode(agenthook.BuildOutput(input, resp))
}

func runLegacyAgentHook(
	ctx context.Context,
	opts agenthook.Options,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error {
	input, err := agenthook.DecodeInput(stdin)
	if err != nil {
		return fmt.Errorf("decode agent-hook input: %w", err)
	}
	if input.SessionID == "" {
		return fmt.Errorf("agent-hook input missing session_id")
	}
	resp, err := postAgentHook(ctx, agenthook.Request{
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
	hookBinary := ""
	opts := agenthook.InstallOptions{
		Timeout: 10 * time.Second,
	}
	cmd := &cobra.Command{
		Use:                   "install",
		Short:                 "Install hooks for detected or selected coding agents",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if hookBinary != "" && opts.Command != "" {
				return fmt.Errorf("--binary and --command cannot be used together")
			}
			if opts.Command == "" {
				resolution, err := githook.ResolveRoborevPath(hookBinary)
				if err != nil {
					return fmt.Errorf("resolve roborev binary: %w", err)
				}
				opts.Executable = resolution.Path
				if resolution.Notice != "" {
					fmt.Fprintln(cmd.OutOrStdout(), resolution.Notice)
				}
			}
			return agenthook.RunInstall(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.Agent, "agent", opts.Agent, "agent profile to update; empty detects installed agents, all updates every profile")
	cmd.Flags().StringVar(&opts.Command, "command", opts.Command, "hook command to install; defaults to this binary plus 'agent-hook run'")
	cmd.Flags().StringVar(&hookBinary, "binary", "", "roborev binary path to bake into agent hooks (for version-manager shims)")
	cmd.Flags().StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "hook config path for a single selected agent")
	cmd.Flags().Var(&agentHookSecondsOrDuration{d: &opts.Timeout}, "timeout", "hook timeout (e.g. 10s, 1m, or bare integer seconds)")
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
			if opts.Command == "" {
				resolution, err := githook.ResolveRoborevPath("")
				if err != nil {
					return fmt.Errorf("resolve roborev binary: %w", err)
				}
				opts.Executable = resolution.Path
				if resolution.Notice != "" {
					fmt.Fprintln(cmd.ErrOrStderr(), resolution.Notice)
				}
			}
			return agenthook.RunDump(opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.Agent, "agent", opts.Agent, "agent profile to dump")
	cmd.Flags().StringVar(&opts.Command, "command", opts.Command, "hook command to install; defaults to this binary plus 'agent-hook run'")
	cmd.Flags().StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "config path to read and merge into; defaults to the agent's standard path")
	cmd.Flags().Var(&agentHookSecondsOrDuration{d: &opts.Timeout}, "timeout", "hook timeout (e.g. 10s, 1m, or bare integer seconds)")
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

func runAgentHook(
	agent kitagenthook.Agent,
	opts agenthook.Options,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error {
	return runHook(agent, opts, stdin, stdout, stderr)
}

func runHook(
	agent kitagenthook.Agent,
	opts agenthook.Options,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error {
	return kitagenthook.Handle(
		context.Background(),
		agent,
		stdin,
		stdout,
		newRoborevAgentHookHandler(agent, opts, stderr),
	)
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
