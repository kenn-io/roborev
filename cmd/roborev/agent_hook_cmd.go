package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/spf13/cobra"
	kitagenthook "go.kenn.io/kit/agenthook"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/version"
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
		agentHookStatusCmd(),
		agentHookResetCmd(),
		agentHookFixDoneCmd(),
	)
	return cmd
}

func agentHookFixDoneCmd() *cobra.Command {
	serverAddr := ""
	cmd := &cobra.Command{
		Use:                   "fix-done <fix-session-id>",
		Short:                 "Complete an Agent Hook fix session",
		Args:                  cobra.ExactArgs(1),
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentHookFixDone(cmd.Context(), args[0], serverAddr, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(
		&serverAddr, "roborev-server", "", "roborev daemon address; defaults to runtime discovery",
	)
	return cmd
}

func runAgentHookFixDone(ctx context.Context, rawID, serverAddr string, stdout io.Writer) error {
	fixSessionID, err := uuid.Parse(rawID) //nolint:forbidigo // Agent Hook fix session ID CLI text boundary.
	if err != nil {
		return fmt.Errorf("parse fix session ID: %w", err)
	}
	if serverAddr == "" {
		if err := agentHookEnsureDaemon(); err != nil {
			return err
		}
	}
	if err := postAgentHookFixDoneRequest(ctx, serverAddr, fixSessionID); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Completed Agent Hook fix session %s.\n", fixSessionID)
	return err
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
			if source != agenthook.RegistrationSource {
				return fmt.Errorf(
					"agent hook registration is outdated; remove this hook from your agent config, then run 'roborev agent-hook install'",
				)
			}
			rawAgent := strings.ToLower(strings.TrimSpace(agent))
			if rawAgent == "" {
				return fmt.Errorf("--agent is required")
			}
			resolved, err := agenthook.ResolveOptionsForAgent(rawAgent, opts, agentHookFlagChanges(cmd))
			if err != nil {
				return err
			}
			if selected, ok := agenthook.LocalProfile(rawAgent); ok {
				return runLocalAgentHook(selected, resolved, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
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

// runLocalAgentHook runs the hook for an agent profile this repository
// implements without a kit profile. These harnesses follow the Claude Code
// stdin and JSON output protocol, so one direct runner covers them.
func runLocalAgentHook(agent kitagenthook.Agent, opts agenthook.Options, stdin io.Reader, stdout, stderr io.Writer) error {
	displayName := "agent"
	if profile, ok := agenthook.LocalProfileDisplayName(agent); ok {
		displayName = profile
	}
	input, err := agenthook.DecodeInput(stdin)
	if err != nil {
		return fmt.Errorf("decode %s input: %w", displayName, err)
	}
	if input.SessionID == "" {
		return fmt.Errorf("decode %s input: missing session_id", displayName)
	}
	instruction := opts.Instruction
	if input.HookEventName == "UserPromptSubmit" || input.HookEventName == "SessionStart" {
		// Injected reminders carry purpose-built guidance in the daemon.
		// The CLI default-fills Instruction with the stop-decision text,
		// which would override it, so only explicitly configured
		// instructions travel for these events.
		if strings.TrimSpace(instruction) == strings.TrimSpace(agenthook.DefaultInstruction) {
			instruction = ""
		}
	}
	resp, err := postAgentHook(context.Background(), opts.RoborevServerAddr, agenthook.Request{
		MCP:                   opts.MCP,
		Agent:                 agent,
		Event:                 input,
		Threshold:             opts.TurnThreshold,
		CommitThreshold:       opts.CommitThreshold,
		FailedReviewThreshold: opts.FailedReviewThreshold,
		Instruction:           instruction,
	})
	if err != nil {
		fmt.Fprintf(stderr, "roborev %s: %v\n", displayName, err)
		return json.MarshalWrite(stdout, map[string]any{})
	}
	if !resp.Triggered && resp.Diagnosis != "" {
		// Harnesses record a hook's error-stream preview, so the evaluation
		// summary is visible when debugging why no reminder was injected.
		fmt.Fprintf(stderr, "roborev %s: %s\n", displayName, resp.Diagnosis)
	}
	if resp.Triggered {
		if resp.TriggeredBy == "fix_session" {
			return json.MarshalWrite(stdout, agenthook.BuildOutput(input, resp))
		}
		if input.HookEventName == "UserPromptSubmit" || input.HookEventName == "SessionStart" {
			// Injected context must stay self-contained: the daemon reason
			// already carries the triage guidance, so only append the fix
			// skill warning and guidelines rather than the stop instruction.
			resp.Reason = prependAgentHookFixSkillWarning(
				agent, opts.MCP,
				agenthook.PostToolUseAdditionalContextWithFixGuidelines(resp.Reason, opts.FixGuidelines),
			)
			return json.MarshalWrite(stdout, agenthook.BuildOutput(input, resp))
		}
		resp.Reason = prependAgentHookFixSkillWarning(
			agent, opts.MCP,
			agenthook.StopReasonWithFixGuidelines(resp.Reason, opts.FixGuidelines),
		)
		return json.MarshalWrite(stdout, agenthook.BuildOutput(input, resp))
	}
	return json.MarshalWrite(stdout, agenthook.BuildOutputWithFixGuidelines(input, resp, opts.FixGuidelines))
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
			if err := agenthook.RunInstall(opts, cmd.OutOrStdout()); err != nil {
				return err
			}
			printStaleDaemonNotice(cmd.OutOrStdout())
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Agent, "agent", opts.Agent, "agent profile to update; empty detects installed agents, all updates every profile")
	cmd.Flags().BoolVar(&opts.MCP, "mcp", false, "install hooks and skills using MCP tools")
	cmd.Flags().StringVar(&opts.MCPTransport, "mcp-transport", "stdio", "MCP transport to install: stdio or http")
	cmd.Flags().StringVar(&opts.MCPURL, "mcp-url", "", "existing daemon /mcp URL for HTTP transport")
	cmd.Flags().StringVar(&opts.RoborevServerAddr, "roborev-server", "", "daemon address for the installed hook and MCP connection")
	cmd.Flags().StringVar(&opts.Command, "command", opts.Command, "hook command to install; defaults to this binary plus 'agent-hook run'")
	cmd.Flags().StringVar(&hookBinary, "binary", "", "roborev binary path to bake into agent hooks (for version-manager shims)")
	cmd.Flags().StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "hook config path for a single selected agent")
	cmd.Flags().Var(&agentHookSecondsOrDuration{d: &opts.Timeout}, "timeout", "hook timeout (e.g. 10s, 1m, or bare integer seconds)")
	cmd.Flags().BoolVar(&opts.StopForm, "stop-form", opts.StopForm, "register ZCode's Stop/PreToolUse/PostToolUse hooks instead of UserPromptSubmit/SessionStart (ZCode does not dispatch those events yet)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", opts.DryRun, "print what would change without writing files")
	return cmd
}

func agentHookDumpCmd() *cobra.Command {
	opts := agenthook.InstallOptions{Timeout: 10 * time.Second}
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
	cmd.Flags().BoolVar(&opts.MCP, "mcp", false, "install hooks and skills using MCP tools")
	cmd.Flags().StringVar(&opts.MCPTransport, "mcp-transport", "stdio", "MCP transport to install: stdio or http")
	cmd.Flags().StringVar(&opts.MCPURL, "mcp-url", "", "existing daemon /mcp URL for HTTP transport")
	cmd.Flags().StringVar(&opts.RoborevServerAddr, "roborev-server", "", "daemon address for the installed hook and MCP connection")
	cmd.Flags().StringVar(&opts.Command, "command", opts.Command, "hook command to install; defaults to this binary plus 'agent-hook run'")
	cmd.Flags().StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "config path to read and merge into; defaults to the agent's standard path")
	cmd.Flags().Var(&agentHookSecondsOrDuration{d: &opts.Timeout}, "timeout", "hook timeout (e.g. 10s, 1m, or bare integer seconds)")
	cmd.Flags().BoolVar(&opts.StopForm, "stop-form", opts.StopForm, "dump ZCode's Stop/PreToolUse/PostToolUse hook form instead of UserPromptSubmit/SessionStart")
	return cmd
}

func agentHookStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:                   "status",
		Short:                 "Print tracked agent hook session counts as JSON",
		Args:                  cobra.NoArgs,
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentHookStatus(cmd.OutOrStdout())
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
			return runAgentHookReset(opts, sessionID, cmd.OutOrStdout())
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
	cmd.Flags().BoolVar(&opts.MCP, "mcp", false, "instruct the agent to use roborev MCP tools")
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

// printStaleDaemonNotice warns when a running daemon is older than this
// binary: installed hooks call the daemon, and a stale daemon silently skips
// events and reminder types it does not know, which surfaced as hooks
// returning empty output during ZCode integration testing.
var printStaleDaemonNotice = func(stdout io.Writer) {
	status, err := fetchDaemonStatus(context.Background(), getDaemonEndpoint())
	if err != nil || status == nil {
		return
	}
	if status.Version == version.Version {
		return
	}
	fmt.Fprintf(stdout,
		"notice: running daemon version %s differs from this binary (%s); run 'roborev daemon restart' so installed hooks reach current reminder behavior\n",
		status.Version, version.Version)
}
