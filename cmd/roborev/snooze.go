package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/daemon"
)

const defaultAgentHookSnooze = 8 * time.Hour

func snoozeCmd() *cobra.Command {
	duration := defaultAgentHookSnooze
	cmd := &cobra.Command{
		Use:   "snooze [on|off]",
		Short: "Temporarily silence agent-hook reminders in this workspace",
		Long: `Temporarily silence agent-hook reminders for the current worktree and branch.

Reviews continue to enqueue and run while the agent hook is snoozed. With no
action, snooze behaves like "on". Use "off" to resume reminders immediately.`,
		Args:                  cobra.MaximumNArgs(1),
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			action := "on"
			if len(args) == 1 {
				action = strings.ToLower(strings.TrimSpace(args[0]))
			}
			if action != "on" && action != "off" {
				return fmt.Errorf("action must be on or off")
			}
			if action == "off" && cmd.Flags().Changed("duration") {
				return fmt.Errorf("--duration cannot be used with snooze off")
			}
			if duration <= 0 {
				return fmt.Errorf("snooze duration must be positive")
			}
			return runSnooze(cmd, action == "on", duration)
		},
	}
	cmd.Flags().DurationVarP(
		&duration, "duration", "d", defaultAgentHookSnooze,
		"how long to silence agent-hook reminders (for example 30m or 8h)",
	)
	return cmd
}

func runSnooze(cmd *cobra.Command, enabled bool, duration time.Duration) error {
	repoPath, worktreePath, branch, err := currentSnoozeScope(cmd.Context())
	if err != nil {
		return err
	}
	if err := ensureDaemon(); err != nil {
		return err
	}

	req := daemon.AgentHookSnoozeRequest{
		RepoPath:     repoPath,
		WorktreePath: worktreePath,
		Branch:       branch,
		Enabled:      enabled,
	}
	if enabled {
		req.SnoozedUntil = time.Now().Add(duration).UTC()
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode agent hook snooze: %w", err)
	}
	ep := getDaemonEndpoint()
	httpReq, err := http.NewRequestWithContext(
		cmd.Context(), http.MethodPost,
		ep.BaseURL()+"/api/agent-hook/snooze", bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("create agent hook snooze request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := ep.HTTPClient(5 * time.Second).Do(httpReq)
	if err != nil {
		return fmt.Errorf("update agent hook snooze: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"update agent hook snooze: daemon returned %s: %s",
			resp.Status, strings.TrimSpace(string(message)),
		)
	}

	label := branch
	if label == "" {
		label = "detached HEAD"
	} else {
		label = fmt.Sprintf("branch %q", branch)
	}
	if enabled {
		fmt.Fprintf(
			cmd.OutOrStdout(),
			"Agent hook snoozed for %s on %s. Reviews will continue to run.\n",
			formatSnoozeDuration(duration), label,
		)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Agent hook resumed on %s.\n", label)
	}
	return nil
}

func currentSnoozeScope(ctx context.Context) (string, string, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", "", fmt.Errorf("get current directory: %w", err)
	}
	worktreePath, err := gitrepo.Root(ctx, cwd)
	if err != nil || worktreePath == "" {
		return "", "", "", fmt.Errorf("not inside a git repository")
	}
	repoPath, err := gitrepo.MainRoot(ctx, worktreePath)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve main repository: %w", err)
	}
	if repoPath == "" {
		return "", "", "", fmt.Errorf("resolve main repository: empty path")
	}
	return repoPath, worktreePath,
		gitrepo.CurrentBranch(ctx, worktreePath), nil
}

func formatSnoozeDuration(duration time.Duration) string {
	if duration%time.Hour == 0 {
		return fmt.Sprintf("%dh", int64(duration/time.Hour))
	}
	if duration%time.Minute == 0 {
		return fmt.Sprintf("%dm", int64(duration/time.Minute))
	}
	return duration.String()
}
